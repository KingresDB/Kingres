# Independent review of the fixes P7F-01 to P7F-07 (branch p7f-fix)

## 1. Scope, method, verdict

Reviewed: commits 5ca7e42710, b2431b4170, 5788f7df3a and f13ae261cf on top of d9f79e78d2, against the P7R-fixes review (findings P7F-01 to P7F-07) and section 17 of the ledger phase 7 design. I read the full diff (credentials sync, gate, job, agent, install classes, setFatal, verifier release/restore code, restore subject) and ran:

- Go tests on Windows: control-plane packages ledgeranchor, ledgerstate, ledgerround, internalapi, cmd/kingresd, and the verifier `go test ./...`. All pass.
- 18 mutants (section 4), each run against the package tests.
- No PostgreSQL-backed run: the fixes touch no SQL, and the new tests use in-memory stores. This is a limit of the review.

Verdict: all seven findings are fixed in the sense that the reported scenario no longer happens and the key guards are isolated by a test. No critical or high finding. New findings: 2 medium, 4 low, plus test gaps (section 5).

## 2. Verdict for each finding

| ID | Verdict | Evidence |
|---|---|---|
| P7F-01 holders compared by digest, redelivery, take-newer, alert, gate | Fixed, with limits (P7G-02, P7G-03, P7G-05) | Mutants M1, M2 (age judging), M3 (cache without term), M4 (no cache), M5 (alert not carried while a holder is unreached), M11 (gate error ignored), M15 (digest without id): killed. M6, M7, M12, M14 survive (section 5). |
| P7F-02 install failure classes | Fixed, one class left wrong (P7G-04) | `ErrInstallWrite` gives retryable `credentials_unreached`; the agent answers 500 `credentials_not_written`. Read and tested; a passphrase file that cannot be read is still final. |
| P7F-03 survivors and dead note | Fixed | O2, P1, P5 have tests; the dead success note is gone (read). |
| P7F-04 dry-run set | Fixed as specified; see P7G-06 | Mutant V1 (date only) killed. V2 (commit only) survives: the test marks a set only when both fields are unknown, or the commit-only case is not isolated. |
| P7F-05 setFatal by highest term | Fixed | Mutant M9 (first error wins) and M10 (`>=`) both killed by `TestAnErrorOfALaterTermIsNotHiddenByAnEarlierOne`. Equal terms keep the first error, a late earlier-term error never replaces a later one: correct by reading. |
| P7F-06 unread malformed restore events | Fixed; verdict cannot become a false VERIFIED | `Restores` and `UnreadRestores` are used only for notes and failure remarks (`cli_restore.go`); no code path turns a failed comparison into a pass. A forked or tampered log fails through the checkpoint, consistency and leaf checks as before, with or without a restore event. Mutants V3 (unread not recorded) and V4 (remark without unread events) killed. See P7G-01 for a related problem. |
| P7F-07 subject format | Fixed in the publisher; breaks against the verifier (P7G-01) | `generation:<n>:<16 hex of the replaced state sum>`. Mutant S1 (subject without the sum) killed by 2 tests. Re-announcing the same restore gives the same subject. |

## 3. Answers to the focus questions

**Leak through the digest, logs, errors, internal API.** No secret and no value is logged: the digest is not written to any log or error text, `errText` is used for all error paths, and `CopyInfo` holds only id, digest and a time. The digest is SHA-256 over a fixed label, the id and the canonical JSON of the plaintext members, the same for plain and sealed files. It is neither salted per file nor keyed. A holder of the digest can test guesses offline. For a WORM key pair (access key id plus a 40 character secret) this is not feasible. For a TSA password chosen by a person, or a short secret that the minimum length (anchor.MinSecretLength) allows, it is feasible. Exposure is limited: the call is leader-only on the internal mutual-TLS API, and a peer that can call it can already call `credentials/fetch` and read the file. Still, a digest is a weaker thing to leak into a future log or a metric than a revision number (P7G-05). Constant-time comparison is not used (`o.Digest == own.Digest`); this does not matter, because the digest is not an authenticator and both sides are authenticated peers.

**Ordering by file write time.** The time is the file mtime on each server (`os.Lstat`), compared strictly with `After`.

- Clock skew: a holder whose clock is ahead by s seconds shows a file written within s seconds after a later rotation as newer; the leader then takes the older file from the holder and distributes it, reverting the rotation silently (a Warn log, no alert). The design admits this limit.
- A file restored from backup: restore tools and `cp` without preserve give the file the restore time as mtime. That file is then the newest copy in the cluster although it is the oldest credential. The leader takes it and distributes it to every holder (P7G-02). A restore that keeps the old mtime is harmless.
- Two rotations close together: with no skew, holders write after the leader, so their mtime is later than the leader's by the delivery time, but the next rotation request starts after the previous response, so the order holds. Concurrent updates to the same target can break it (not run).
- No loop between holders: the judgement uses strict `After` on two absolute times, so it is antisymmetric; only the leader acts, and the file the leader installs gets the leader's current time, after which digests are equal.

**Can the gate stop anchoring for good.** Holders that cannot be asked do not stop anything (right call after a failover). A newer file that cannot be taken gives retryable `credentials_stale`, an alert from the job (critical when the target is enabled), and the record waits. Two ways this lasts: the retry limits (`maxRetryAttempts`, `maxRetryAgeHours`) apply to this class like to every retryable class, so a newer file that stays untakeable for longer turns the records into failed ones; and a newer file that this server can never open (mixed sealed and plain servers, different passphrases) is retryable forever (P7G-04). A holder with a skewed clock that is permanently ahead does not stop anchoring; it causes the revert of P7G-02 instead.

**The 30 s cache per target and term.** Correct across a term change: the key holds the term, so a new term asks again (mutant M3 killed). Observations: a cache entry is written also when the holders could not be asked at all, so a transient network error at the start of a term gives 30 s of anchoring with the own file, as designed; an error of the gate is not cached, so every pass asks all holders again; the map is never pruned (one small entry per target and term, a slow leak, P7G-06).

## 4. Mutants

| Id | Change | Result |
|---|---|---|
| M1 | `judgeCopies`: never newer | killed (4 tests) |
| M2 | `judgeCopies`: Before instead of After | killed (7) |
| M3 | gate key without the term | killed (1) |
| M4 | gate without the cache | killed (1) |
| M5 | alert not carried while a holder is unreached | killed (1) |
| M6 | `delivered` never set | survived |
| M7 | stale list not recomputed after take-newer | survived |
| M9 | `setFatal`: first error wins | killed (1) |
| M10 | `setFatal`: `term >= fatalTerm` | killed (1) |
| M11 | gate: error of take-newer ignored | killed (1) |
| M12 | gate: holders error does not skip the judgement (designer's equivalent mutant) | survived; equivalent, as claimed (a failed report holds no copies) |
| M14 | gate: no own file gives credentials_stale | survived (no test runs the worker for a target without a file while holders are set) |
| M15 | digest without the target id | killed (1) |
| S1 | restore subject without the sum | killed (2) |
| V1 | release: only the date marks a dry run | killed (1) |
| V2 | release: only the commit marks a dry run | survived |
| V3 | unread restore events not recorded | killed (2) |
| V4 | failure remark ignores unread events | killed (1) |

M8 and M13 were my own pattern errors and are not counted. M5 and M12 first failed to compile, and were repeated in a form that compiles. All mutants were restored; the worktree was clean when it was removed.

## 5. New findings

### P7G-01 (medium): the fix of P7F-07 produces events that the verifier refuses to read

Place: `kingres/verifier/internal/events/restore.go` (the loop `for _, prev := range w.Restores { if prev.Generation == r.Generation`), `kingres/control-plane/internal/ledgerround/restore.go` `restoreSubject`.

Scenario. P7F-07 exists for a record that was lost and made again from the database, so that a second restore reaches the same generation number. The publisher now announces it (different subject). The verifier takes the strict rule "a generation is acknowledged once": the second event goes to `UnreadRestores` with the reason "acknowledged twice", and the commands print "finding ... not read: nothing of it is used", and a failed comparison says "this run cannot say whether it explains the failure". The existing test `TestAMalformedRestoreAcknowledgedEventIsNotReadAndTheRunGoesOn` shows this output for "the same generation twice". So the publisher and the verifier now disagree on exactly the case the fix was made for. No wrong accept results, but the second legitimate cut is never explained to an auditor.

Fix: decide one rule. Either the verifier accepts the same generation with a different `published-state-sha256` (reject only the same generation and the same sum), or the publisher takes a counter that cannot repeat. Add a cross test from the publisher output to the verifier for two cuts of one generation.

### P7G-02 (medium): an old credentials file with a new mtime replaces the current one

Place: `credsync.go` `judgeCopies` (lines 82 to 99), `CredStore.Digest` (the time is the file mtime), `jobCredentials` take-newer branch.

Scenario. Three holders, rotation X2 done and distributed. One holder is restored from a backup of the key directory made before the rotation, with the restore time as mtime (a normal restore, and the code already expects "a host restored without its key directory"). Its file X1 differs from the leader's and is newer by mtime. The leader takes X1 (`takeNewer`) and gives it to every holder. The rotation is reverted cluster-wide; the old key may have been revoked after a leak, and anchoring then fails; the only trace is a Warn log. The same happens with a holder whose clock is ahead within the time between two rotations. Not run end to end; the judging function and the install path were read and the unit mutants confirm that the time alone decides.

Fix: do not let a mtime alone move credentials backwards: (a) a counter in the target row written at every change (as the design already names as the next step), or (b) store the time of the change inside the file (inside the sealed body or the authenticated data) so a copy keeps it, and compare that, never the file system time; and (c) raise an alert, not only a Warn, when the leader takes a file from a holder.

### P7G-03 (low): the time that a holder reports is trusted without a bound

Place: `judgeCopies`.

A holder (or a server with a wrong clock) that reports a time in the far future always wins. The leader then takes whatever file it serves for that target, installs it and distributes it. A peer that has the cluster certificate can already read the secrets, but it could not before steer the credentials of the leader. Fix: ignore (and alert on) a time later than the leader's now plus a bound for the clock error; with P7G-02(b) the problem disappears.

### P7G-04 (low): install and take-newer errors of a local kind are classed final or waiting forever

Place: `creds.go` `Install` / `Decode` / `passphrase`, `credsync.go` `fetchMissing` and `ensureCurrent`.

(a) `Decode` returns `ErrCredentials` also when the passphrase file of this server cannot be read (permissions, I/O error, a mount that is not yet there). `fetchMissing` takes that for "no other holder could give it" (`fetchNone`, final, text "enter the credentials again"), which is the class that P7F-02 removed for a full disk. (b) `takeNewer` wraps every install error into `credsStaleError`, retryable, so a file that this server can never open (a plain server, a sealed file of another passphrase) makes the gate wait until the retry limits turn the records into failed ones; the alert text of the job says "asked again every minute", which is not true then. Fix: give the passphrase-file read failure the same class as `ErrInstallWrite`; in `takeNewer` tell "cannot open here" (final, operator action, text that says which) from "cannot write".

### P7G-05 (low): the digest is neither salted nor keyed

Place: `creds.go` `Digest`.

See section 3. If the digest ever reaches a log, a metric or a support bundle, a low-entropy password (TSA) can be found by guessing offline. Fix: do not use a content hash for the comparison; use the counter of P7G-02, or an HMAC with a key derived from a secret that all holders share (the signing key material or the passphrase) and the target id; compare with `subtle.ConstantTimeCompare`.

### P7G-06 (low): small defects

- `credGate` is never pruned (entries of deleted targets and old terms stay): `credsync.go` `ensureCurrent`. Prune entries of other terms when a term changes.
- A dry-run release set (P7F-04) ends with exit status 0 and the word `VERIFIED (NOT A RELEASE)`. A script that tests only the status takes it for a release. The decision (status 0) was taken on purpose, and it is stated in the design; consider a distinct non-zero status, or a flag `--require-release`.
- Test gaps that the survivors show: M6 (the alert is kept when a pass delivered but a holder was unreached; harmless, conservative), M7 (a holder between the leader's old file and the newer one is served one pass late), M14 (a target without a file must still reach `fetchMissing` through the gate: a regression here would make every failover to a server without the file permanent), V2 (a commit-only unknown is not isolated).

## 6. Hygiene

The worktree was removed without force; no mutant remains; nothing was committed or pushed; no PostgreSQL install was made, so there is nothing to delete.
