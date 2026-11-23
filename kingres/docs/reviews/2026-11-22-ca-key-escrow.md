<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026 RooyeKhat Media Co -->

# Security review: escrow of the CA key (branch ca-escrow, 7fa33d82d2..a16f78362d on 908518fa05)

Scope: kingres/control-plane internal/pki/escrow.go, internal/membership/escrow.go, the three internal calls, the three
REST operations, the failover hook, and section 3.4.1 of kingres/docs/design/ha-phase5-part2.md.

## Verdict on the threat model as written

The construction is sound and the threat model is honest about its main point: a recipient's disk holds both the copy
and the key that opens it, so the set of disks that can hold the CA key grows to the leader plus every ready standby of
group 0. The cryptography is correct: ephemeral-static X25519, HKDF-SHA256 with both public keys as salt and a fixed
info label, AES-256-GCM under a fresh ephemeral key (so the key is unique per seal and the random 96-bit nonce cannot
repeat under one key), and additional data that cover version, recipient, key id, CA fingerprint, generation and time.
A recipient accepts only a key that matches its own ca.crt, so a forged blob cannot swap the CA. The escrow private key
is never sent. I found no path by which a secret reaches a log, an error, a REST answer or an audit event.

The model understates three things, all about state and not about the cipher:

1. The "never older" rule is the only rollback defence. It compares generation and time, not content, and a new leader
   reuses the generation it adopted (CAE-1).
2. The leader's record of who holds a copy is lost or discarded in two places, so "a server that stops being a
   recipient is told to delete" and "switching off removes every copy" are not guaranteed (CAE-2, CAE-3).
3. Threat 7 (an old leader returns with ca.key and a stale revoked.json) is worse than "stale": it can undo
   revocations cluster-wide (CAE-4).

No finding lets a network attacker or a non-leader store, drop or fetch a copy. The three calls are LeaderOnly, the
catalog leader is compared with the TLS peer, and the receiving side re-checks recipient, key id, AEAD tag and CA key.

## Findings

### CAE-1 (medium) A new leader reuses the adopted generation; a stale blob can win, or the new leader's blobs can be refused
internal/pki/escrow.go:504-509 (adopt keeps `st.Generation = b.Generation`), :381-385 (store rule),
internal/membership/escrow.go:323.

Scenario A (rollback). The old leader L1 is demoted but not yet aware of it (lease change, split-brain window). It
still holds ca.key. It seals its older revocation list at generation g with a later SealedAt than the copy of the new
leader L2 (also generation g, because L2 adopted g). The store rule accepts it (same generation, newer time), so the
recipient's copy now carries the older list. The union with revoked-serials.json limits the harm to revocations not yet
distributed, but a later leader that adopts that copy and has a stale local list loses them. The escrow calls carry no
fence token (internalapi.go:504-537; fenceRequired does not list them).

Scenario B (availability). L2's clock is behind L1's by d. L2 reuses generation g, so its SealedAt is up to d older than
the copy held and every other recipient refuses with "older than the copy held" (shown as `refused`) until d has passed
or the operator rotates. Escrow looks broken right after a failover, when it matters.

Fix: on adopt, set the generation to b.Generation+1 and persist it before the first send. Put the leader's epoch or
lease term in the additional data and in the ordering key (term, generation, time). Fence the escrow calls with the
token the other leader-only calls use. Never order by wall time alone.

### CAE-2 (medium) The leader forgets a recipient even when the drop failed
internal/membership/escrow.go:308-315.

`_ = m.dropEscrowAt(ctx, id); delete(st.Recipients, id)` deletes the record whatever the result. A recipient that is
unreachable for a moment when its standby is rebuilt, or when the member is removed, keeps the copy and the escrow key
and is never told again. The switch-off path (lines 297-300) does retry; this path does not. Threat 3 says the server
"keeps the copy"; it does not say the leader will never try again.

Fix: keep the record (state `dropPending`) until the drop succeeds, retry each round, show it in getCaEscrow.

### CAE-3 (medium) After adoption the new leader has an empty recipient list, and adoption switches escrow back on
internal/pki/escrow.go:503-509.

`st.Enabled = true` and `st.Recipients = {}` on adopt.
(a) Servers that hold a copy from the old leader and are no longer candidates are not in the new list, so they are never
dropped. setCaEscrow(false) drops only listed recipients, so those copies survive an explicit switch-off.
(b) If the operator had switched escrow off and one drop had failed (server down), that server's stale copy is adopted
at the next failover and escrow is turned on again against the operator's decision, re-sealing the CA key to more disks.

Fix: carry an `enabled` flag and the recipient set in the sealed plaintext (or the system database) and have the new
leader reconcile with it. Refuse to re-enable on adopt when the flag is false. On switch-off send drop to every active
member, not only to recorded recipients (drop is idempotent).

### CAE-4 (high) An old leader that returns with ca.key and a stale revoked.json can unrevoke certificates cluster-wide
internal/pki/pki.go:190-201 (LoadCA reads only revoked.json), internal/membership/add.go:845-851 (distribute overwrites
every server's list through /pki/install, SaveRevokedList replaces), internal/pki/escrow.go:441-443 (AdoptHeld skips the merge).

Scenario: L1 holds ca.key and revoked.json R1. A failover happens. L2 adopts, revokes serial S (R2 = R1 + S) and
distributes R2. Then L1's server becomes the manager leader again (a second failover back to it is a normal outcome when
L2 is the one that fails). CheckAuthority finds ca.key, so adoption returns AdoptHeld and does not run. L1 loads R1 only;
it ignores the newer revoked-serials.json it received as a follower. On its next revocation or CRL publication it
distributes R1 + x to every server, replacing their lists. S is trusted again, and the revoked certificate is accepted by
the mutual-TLS internal API.

Fix: when a server becomes leader (AdoptHeld included), merge revoked-serials.json into revoked.json before it serves
anything. Make the follower's /pki/install take a union with a monotonic list number instead of overwriting. Remove or
quarantine ca.key on a server that stops being leader. The design lists that last as an open point; it is a security
item, not a convenience.

### CAE-5 (medium) ca.key is written without fsync; a crash leaves a truncated key that blocks adoption for good
internal/pki/escrow.go:497, internal/pki/pki.go:154-164 (writeNew: no Sync, no directory sync).

A power loss between create and flush leaves ca.key present but empty or short. The next AdoptEscrow sees the file
(AdoptHeld) and answers "key held", so the alert is not raised, while LoadCA fails on every issue. O_EXCL and 0600 are
right; durability is missing, and revoked.json was written first, so the pair can be inconsistent.

Fix: write ca.key with WriteFile (temp, fsync, rename, directory sync) after checking under the escrow lock that no
ca.key exists, or treat an unparsable ca.key as absent (move it aside, then adopt).

### CAE-6 (low) A transient empty answer from the standby list drops every copy
internal/membership/escrow.go:304-315, cmd/kingresd/mapfailover.go (wireEscrow).

Recipients come from Standbys(0) with state `ready`. A standby that is rebuilding, or a read that returns an empty list
for a moment, makes the server a non-candidate; its copy and escrow key are deleted in the same round. A failover in that
window finds no copy and raises the critical alert.

Fix: drop only after N consecutive rounds as a non-candidate, or only when the server is no longer an active member.

### CAE-7 (low) StoreEscrow does not parse the revocation list, and an unreadable held blob disables the rollback check
internal/pki/escrow.go:378-385.

Only the CA key is validated on store. A blob with an unparsable `revoked` field (only a CA-key holder can make one) is
stored and fails at adoption, after the failover. If the held blob is unreadable, readBlob returns an error and the
"older than held" check is skipped (`err == nil` guard), so any blob replaces it.

Fix: parse `revoked` on store; treat an unreadable held blob as generation 0 and log it.

### CAE-8 (low) Hardening notes
- The AAD joins fields with `|`; a ServerID or KeyID containing `|` could shift fields. Ids are generated and key ids are
  hex, so it is not exploitable; length-prefix the fields.
- Nothing is zeroed (private key bytes, PEM plaintext). Go gives no guarantee, but the plaintext buffer in SealCA and
  `recipientKeyFile.Private` could be cleared.
- Escrow key files are read without a mode check (mutant M12 survives).
- A server that left cleanly loses its pki directory (internal/cluster/member.go:487), so its copy goes too. An
  unreachable removed server keeps `escrow/` (CAE-2). `escrow/state.json` stays on a former leader after DropEscrow.
- getCaEscrow (kingres_admin) lists which servers hold a copy, their key ids and the last error text. An administrator
  already has the standby list; no harm seen, and no key material is in the error text.
- The adopted list carries the CRL number of the last copy, so a new leader may publish a CRL number lower than one
  already published; PostgreSQL does not check it.
- Split brain: two servers that both pass B8 would both write ca.key (same key, diverging revoked.json). Adoption is
  irreversible and is not undone if the failover is abandoned. This matches the existing split-brain exposure of the
  map group; lease fencing is the control, and the CAE-4 fix covers the revocation side.

## Checked and found sound
- Nonce: random 12 bytes under a key that is unique per seal. The constant-nonce mutant survives for that reason: a test
  gap, not a flaw.
- HKDF: salt is ephemeral || recipient public key, info is a fixed label, 32-byte output. No test pins the salt.
- Files: WriteFile (CreateTemp, chmod 0600, fsync, rename, directory sync on Linux) for escrow keys and the sealed copy;
  O_EXCL and 0600 for ca.key; escrow directory 0700.
- Authorization: offer, store and drop are LeaderOnly. A revoked or removed server is not a candidate, and is told to drop
  through the unchecked path only by the leader. The REST operations need kingres_admin and mutating ones forward to the
  leader.
- The escrow private key never leaves the server; the offer carries only the public key and its id.

## Tests and mutants

Unit tests on Windows (internal/pki, internal/membership): pass. Cluster test result: see the last section.

Mutants, applied one at a time to the committed source, run against the tests of pki, membership and internalapi, each
restored afterwards (worktree clean at the end):

| Id | Change | Result |
|---|---|---|
| M1 | AAD without generation and time | killed (TestHostileBlobs) |
| M2 | AAD without CA fingerprint | SURVIVED (also checked against ca.crt, so redundant, but no test relabels it) |
| M3 | store skips the key-matches-CA check | killed (TestABlobForAnotherCAIsNotAdopted) |
| M4 | store skips the older-than-held check | killed (TestAnOlderBlobDoesNotReplaceANewerOne) |
| M5 | adopt skips the union with revoked-serials.json | killed |
| M6 | adopt ignores an existing ca.key | killed |
| M7 | adopt skips the CA fingerprint and key check | killed |
| M8 | drop is a no-op | killed |
| M9 | constant nonce | SURVIVED (harmless here, see above) |
| M10 | HKDF without salt | SURVIVED |
| M11 | old escrow keys kept after store | killed |
| M12 | escrow key file mode 0644 | SURVIVED (gap) |
| M13 | non-candidates are not dropped | killed |
| M14 | candidates not limited to active members | killed |
| M15 | SyncEscrow ignores who the leader is | killed (TestAFormerLeaderThatStillHoldsTheKeySendsNothing) |
| M16 | store call ToLeader instead of LeaderOnly | killed (TestEscrowCallsAreLeaderOnly) |
| M17 | switching off keeps the copies | killed |
| M18 | no resend after a revocation | SURVIVED (gap: no unit test shows a revocation reaches the copies) |
| M19 | open skips the ServerID check | killed |
| M20 | resend ignores a changed content hash | killed |

Test gaps to close: key file mode (M12), resend after revocation (M18), AAD fingerprint relabel (M2), and tests for
CAE-1 to CAE-5 (same generation with newer time and older list; failed drop is retried; adopt after switch-off; returning
old leader with stale revoked.json; truncated ca.key).

## Cluster test (TestCAEscrow, lane l4)

TestCAEscrowMapGroupFailover PASS (203.7 s) and TestCAEscrowTamperedCopyIsNotAdopted PASS (137.9 s) on lane l4 with a private install. Both tests cover only the happy failover and a tampered copy; none covers a returning old leader, a stale-but-valid copy, a failed drop, or switch-off after a failover (CAE-1 to CAE-4).

## Fixes (2026-11-23, branch ca-escrow)

Each fix has a test that fails without it (the mutant that was run is named). Unit tests are in `internal/pki` (`escrow_test.go`,
`escrow_review_test.go`), `internal/membership` (`escrow_test.go`, `escrow_review_test.go`) and `cmd/kingresd`
(`mapfailover_authority_test.go`); the cluster tests are in `internal/clustertest/caescrow_test.go`.

| Finding | Fix | Test (mutant) |
|---|---|---|
| CAE-4 (high) | A revocation list never shrinks. (1) A holder refuses a list whose number is lower than the one it holds, or that lacks a serial the held one has (`pki.CheckNotStale`, 409 `stale_revocations`), and its serial list is a union. (2) A server that holds `ca.key` and receives a list merges it into its own CA (`CA.MergeRevoked`), so a former leader's list is not stale when it is leader again. (3) A leader that takes over, with or without adopting (`AdoptEscrow`), and every distribution (`distributeRevocations`, the revoke task) first merges the list of its own file, of its sealed copy, of the published list and of every server (`GET /revoked`), and raises the number of its next list above the published one. (4) A refusal makes the sender merge and send again (`installRevocations`). (5) The number of a list is at least the time in seconds, so the lists of two holders of the key are in the order they were issued in. | `TestAHolderRefusesAListThatTakesARevocationBack`, `TestAFormerLeaderThatReceivesAListTakesItIntoItsCA`, `TestAReturningLeaderMergesWhatTheServersHoldBeforeItDistributes`, `TestADistributionAsksTheServersFirst`, `TestALeaderWhoseListWentBackIsRefusedAndRepairs`, `TestACRLThatTakesARevocationBackIsStale`, `TestMergeRevokedAndTheCRLNumber`, `TestAdoptionKeepsThePublishedCRLNumber`; cluster `TestCAEscrowARevocationAfterTheFailoverHoldsWhenTheOldLeaderReturns` (mutants: no stale check, no merge into the CA on install, list overwritten instead of united, no reconcile at adoption, none before a distribution, no repair) |
| CAE-1 (medium) | The order of the copies is (epoch, generation, seq); the wall clock orders nothing, so a leader whose clock is behind is not refused and a former leader's blob is. A leader that takes over (adopt or return) takes an epoch above every one it has seen (the system database, its own copy) and a generation above them. A recipient refuses a blob, and a drop, of an older epoch (409 `stale_epoch`); a leader that finds a copy of a higher epoch on a server seals nothing over it. Only the operator's rotation, made on the leader of the moment, takes an epoch above what a server holds. | `TestAFormerLeadersBlobIsRefusedWhateverItsClock`, `TestPositionOrder`, `TestAFormerLeaderCannotOverrideANewerOne`, `TestTheOperatorsRotationTakesAnEpochAboveWhatTheServersHold` (mutants: ordering by time, no epoch bump, no refusal over a newer epoch, drop not fenced, rotation without a bump) |
| CAE-2 (medium) | A recipient's record stays, as `dropPending`, until the drop succeeded; it is repeated every round and shown in `getCaEscrow` (`state: dropPending`, contract 0.22.0). | `TestAServerThatIsNoLongerAMemberLosesItsCopy` (mutant: the record is forgotten after a failed drop) |
| CAE-3 (medium) | The operator's choice (`enabled`), the generation, the epoch and the servers that hold a copy are kept in the system database (`kingres_cp.cluster_setting`, key `caEscrow`), which follows the map group. The new leader reads them: it keeps the escrow off when the operator switched it off (a stale copy does not switch it on again), drops the copies of servers that no longer qualify, and, with no record, takes a copy to mean the escrow was on. Switching off tells every active member to delete a copy, not only the recorded ones. | `TestAStaleCopyDoesNotSwitchTheEscrowBackOn`, `TestTheNewLeaderDropsTheCopiesOfServersThatNoLongerQualify`, `TestWithoutARecordACopyMeansTheEscrowWasOn`, `TestTheStateIsMirroredToTheSystemDatabase` (mutants: no read of the record, no holders, no mirror, a sweep of the recorded servers only) |
| CAE-5 (medium) | `ca.key` is written with the durable write (temp file, fsync, rename, directory fsync) after `revoked.json`. A `ca.key` that is there but is not the key of `ca.crt` is put aside (`ca.key.unusable-<time>`) and the copy is adopted, or, with no copy, the server answers that it has no key; B8 takes the answer of the adoption, not the existence of the file. | `TestATruncatedCAKeyIsReplacedByTheCopy`, `TestATruncatedCAKeyWithoutACopyIsNoKey`, `TestTheAdoptedKeyIsFlushedToDisk`, `TestATruncatedCAKeyOfTheNewLeader`, `TestCheckAuthorityIsTheAnswerOfTheAdoption` (mutants: a truncated key counts as held; a plain write) |
| CAE-6 (low) | A standby that is missing from the list is dropped after three rounds in a row; a server that is no longer an active member at once. | `TestAStandbyThatIsMissingForAMomentKeepsItsCopy` (mutants: no grace, the count never reset) |
| CAE-7 (low) | A blob is stored only if its revocation list parses; a held copy that cannot be read counts as the oldest position. | `TestACopyWithAnUnreadableRevocationListIsRefused`, `TestAnUnreadableHeldCopyDoesNotBlockAGoodOne` (mutant: no parse) |
| CAE-8 (low) | The additional data are length-prefixed; an escrow key file that others can read is refused; the plaintext, the content key, the shared secret and the key bytes are cleared where Go allows; the CRL number never goes back (above). `escrow/state.json` on a former leader stays (it is the bookkeeping, not a secret). | `TestAdditionalDataFieldsDoNotShift`, `TestAnEscrowKeyFileThatOthersCanReadIsRefused` (Linux; mutant M12), `TestAdoptionKeepsThePublishedCRLNumber` |
| M2, M9, M10 (test gaps) | The tag covers the CA fingerprint; two seals share no nonce or ephemeral key; an independent HKDF-SHA256 derivation opens a blob. | `TestTheTagCoversTheCAFingerprint`, `TestTwoSealsDoNotShareANonce`, `TestTheKeyIsDerivedAsDocumented` |
| M12, M18 (test gaps) | See CAE-8; a revocation reaches the copies: `TestSwitchingOnSealsACopyToEveryRecipientAndANewLeaderAdoptsIt` seals again after a revocation, and the new test of the distribution (`TestADistributionAsksTheServersFirst`). | |

**What is still open.** A returning former leader keeps its `ca.key` (it is a follower and its list is merged as above, but the key is on its
disk); the removal of that key stays an operator step. A recipient that is removed while unreachable keeps its copy; it stays listed as
`dropPending` and the leader asks again every round. A new leader needs the system database to be readable to know the operator's choice (it
logs a warning and falls back to the copy otherwise). The CA itself is not rotated.

**Found by the cluster test of CAE-4.** After a failover of the map group, every revocation task on the new leader failed at "Send the revocation list to
every server": the list of servers was read with `peerInfos`, which refuses a server whose row has no address, and the row of the server that created the
cluster has none (its address is in the peer directory). The revocation task and the reads of the lists now take the ids of the active members from the
table (`revocationPeers`) and leave the address to the peer client. The cluster test fails without it.

## Fixes of the review of the fixes (2026-11-23, [report](2026-11-23-ca-key-escrow-fixes.md))

The second review found that no revocation is lost in an honest sequence, and found the holes that the new machinery opened. Each fix has a test that fails
without it (the mutant is named); the surviving mutants of the report, N9, N11, N19, N30, N31 and N32, are killed or explained below.

| Finding | Fix | Test (mutant) |
|---|---|---|
| CAF-1 (high) | The leader merges what another server reports through `GET /revoked` only if the serial is one this CA issued (the certificate table and the certificates of the servers), at most 10000 serials and 64 characters each from one server, and it does not read the servers whose certificates are revoked. If the issued serials cannot be read, nothing from a server is merged. What is merged cannot be undone, so the filter is the defence; the sources that are the server's own (its files, its sealed copy, the signed list it holds) are merged as before. The limit of `POST /pki/install` is 16 MiB, so a list of the allowed size is never refused for its size. | `TestAHostileAnswerCannotMakeTheLeaderRevokeWhatItNeverIssued`, `TestTheAnswersOfARevokedServerAreNotRead`, `TestOneServerCannotMakeTheLeaderMergeAnUnboundedList`, `TestNothingIsMergedFromAServerWhenTheIssuedSerialsCannotBeRead` (mutants: no filter, no count cap, no length cap, revoked servers read) |
| CAF-2 (medium) | The number of a list does not depend on the clocks of the holders any more than it must: `GET /revoked` returns the number the server holds (`crlNumber`), the refusal (409) says it (`held number N`), and the leader raises its next number above every number it learns, from the answers before it sends and from the refusal when it was not asked. A number more than a day above the leader's clock is logged and not followed (a holder with a wrong date does not pin every later list above it); the distribution to that holder fails and the others go on. | `TestALeaderLearnsTheNumberOfAHolderThatIsAheadOfItsClock` (both ways), `TestANumberFarAboveTheClockIsNotFollowed`, `TestTheHeldNumberIsReadFromTheRefusal`, `TestTheNextNumberIsAboveTheListInTheOwnFile` (mutants: not learned from the answer, not from the refusal, no bound, own file ignored) |
| CAF-3 (medium) | The revoke task does not stop at the first server that refuses: errors are collected, the servers that failed are tried once more, and the task fails at the end with what remains. The servers whose certificates are revoked are sent last and what they answer is ignored (and they are not waited for when unreachable). | `TestARefusingServerDoesNotStopTheRevokeTask`, `TestDistributeAllCollectsErrorsAndRetriesOnce` (mutants: stop at the first, a revoked server fails the task, revoked not last) |
| CAF-4 (low) | A rotation does not take an epoch above a server's any more: a leader that finds a copy of a higher epoch seals nothing, rotation included (the real leader would be the one that was refused). Two takeovers that do not see each other's epoch are the split-brain case that the lease fences; the epoch is not a second fence, and this is now said in the threat model. A counter in the system database was not added: the new leader writes it on its own promoted instance, so it orders nothing between two leaders on different timelines. | `TestARotationDoesNotTakeAnEpochAboveWhatAServerHolds` (mutant: rotation takes a higher epoch) |
| CAF-5 (low) | The offer is checked against the epoch it carries when it asks for a new key (rotation): a stale leader cannot make a server pile up escrow keys. | the same test (mutant: offer not fenced) |
| CAF-6 (low) | What is read from the system database is validated: epoch and generation are bounded (no overflow at the next increment), at most 64 holders, each a valid server id; the position that a server reports for its copy must be sane. | `TestTheMirrorIsValidatedWhenItIsRead`, `TestServerIDsAreChecked`, `TestAnImpossiblePositionOfAServersCopyIsRefused` (mutants: unbounded, holders unvalidated, impossible position accepted) |
| CAF-7 (low) | An attempt to delete a copy takes at most 5 seconds (it was 20), and a record that failed 30 rounds in a row is tried every tenth round. | `TestADropThatKeepsFailingIsTriedLessOften` (mutant: always retried) |
| CAF-8 (low) | The mode of the temporary file is set before the flush; `.pki-*` files older than five minutes are removed when a leader takes over and when the escrow round starts; at most three `ca.key.unusable-*` are kept. | `TestTheModeIsSetBeforeTheFlush` (Linux), `TestInterruptedWritesAreCleanedUp` (mutants: no cleanup, mode after the flush) |
| CAF-9 (low) | The list that goes out with a renewed certificate and the one sent to a new server are merged with what the servers hold first, like every other distribution. | `TestAListSentWithACertificateIsAtLeastWhatTheServersHold` (mutant: no merge in the renewal) |
| CAF-10 (low) | A copy of the first version still opens (its additional data are rebuilt the old way), so a failover between an upgrade and the first resend of the new leader finds a working copy. | `TestACopyOfTheFirstVersionStillOpens` (mutant: version 1 not opened) |
| N9, N11, N19, N30 | Test gaps: the sealed copy of a returning leader, the revoke task, the held epoch at adoption and the number of the own file each have a test. | `TestAReturningLeaderMergesTheRevocationsOfItsSealedCopy`, `TestTheRevokeTaskMergesWhatTheServersHoldFirst`, `TestAdoptionTakesTheEpochOfTheHeldCopyAndTheNumberOfTheFile`, `TestTheNextNumberIsAboveTheListInTheOwnFile` |
| N31 (pull path) | Not tested here: the pull path (`InstallRenewed`) skips a list that goes back; it needs a server key and a certificate, which the cluster test has (a renewal with a stale list cannot be made without a second leader). The code is the same `CheckNotStale` as the push path, which is tested. Left open as a test gap. | |
| N32 | Equivalent in the tests: the seam of the tests never lists the leader; the production listing excludes it by id. | |

**What is still open.** The first fix of CAF-1 ignores what this CA did not issue; a compromised member can still make a refused certificate by getting
the leader to merge the serial of a certificate that was issued (for instance the leader's own), but only by a serial that the table holds and only if the
server is not revoked: it is the trust that every member has already (D3). An epoch is not an authority of its own (CAF-4). The pull path has no test of
its own (N31).
