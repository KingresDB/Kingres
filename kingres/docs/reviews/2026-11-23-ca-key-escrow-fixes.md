<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026 RooyeKhat Media Co -->

# Security review: fixes of the CA key escrow review (branch ca-escrow, a16f78362d..6e8e13ddbc)

Scope: commits 53827a20bf, e64aacbaf8, 24a7bb1e4b, 83dbb6a8c1, d26e57093e, 6e8e13ddbc. Read first: the Fixes section of
kingres/docs/reviews/2026-11-22-ca-key-escrow.md, section 3.4.1 of kingres/docs/design/ha-phase5-part2.md and threats 7 to 11,
then the diffs of internal/pki (escrow.go, files.go, pki.go), internal/membership (escrow.go, add.go, certs.go, handle.go),
internal/cluster (member.go, pkirenew.go) and cmd/kingresd/mapfailover.go. Paths below are relative to kingres/control-plane.
Line numbers are those of 6e8e13ddbc.

## Summary

The fixes do what they say for the cases the original review named: a returning leader, a stale blob, a failed drop, a truncated key,
a former leader's clock. I could not make a revocation disappear by an honest sequence of events. The new machinery has one real
hole, found on the way: the leader now merges every serial that any server reports through GET /revoked, without checking that the
serial belongs to a certificate this CA issued, and the merge can never be undone (CAF-1, high). A second problem is the time-based
CRL number, which turns a clock difference between two CA holders into a refusal that the leader cannot learn its way out of
(CAF-2, medium). Everything else is low.

## Verdict per finding of the original review

| Id | Verdict | Remark |
|---|---|---|
| CAE-1 | Fixed, with caveats | Order is (epoch, generation, seq); a stale blob and a stale drop are refused; the leader that finds a higher held epoch seals nothing. Caveats: the offer ignores the epoch it is sent (CAF-5); two leaders can take the same epoch (CAF-4); an operator rotation on a stale leader raises the epoch above the real one (CAF-4). |
| CAE-2 | Fixed | The record stays as dropPending; mutant N21 (forget after a failed drop) is killed. One side effect: a record that can never be dropped stays forever (CAF-7). |
| CAE-3 | Fixed | The operator's choice, epoch, generation and holders are read from the mirror; a stale copy no longer switches escrow on (N25 killed); switch-off sweeps every active member. The mirror is asynchronous state, see CAF-6. |
| CAE-4 | Fixed for the scenario of the review, with a new hole | The refusal rule on the holder, the merge on every holder and the merge before every distribution all work (N1 to N5, N7, N10, N12, N13 killed). The merge itself accepts untrusted input (CAF-1) and the CRL number rule can block distribution (CAF-2). |
| CAE-5 | Fixed | Temp file, fsync, rename, directory fsync on Linux; an unusable key is moved aside; B8 takes the answer of the adoption. Small leftovers in CAF-8. |
| CAE-6 | Fixed | Grace of three rounds; N22 and N23 killed. |
| CAE-7 | Fixed | The list is parsed on store; an unreadable held copy is the oldest position. |
| CAE-8 | Fixed | AAD length-prefixed (N29 with the prefix removed is killed; a mutant that keeps a zero prefix survives, harmless); key file mode checked; buffers cleared. |

## Answers to the questions

### 1. Can a revocation still be lost?

Not by any of the five cases, as long as the serial reaches at least one server that the next leader can reach.

- Returning old leader that becomes leader again: AdoptEscrow reads the sealed copy, revoked-serials.json, the published crl.pem and
  GET /revoked of every active member before anything is sent (membership/escrow.go:217-251), and InstallPKI merges into the CA of
  a former leader (cluster/member.go:178). A holder that sees a list lacking one of its serials answers 409 and the leader merges
  and sends again (escrow.go:257). Works; the cluster test TestCAEscrowARevocationAfterTheFailoverHoldsWhenTheOldLeaderReturns passed.
- Two holders of the key, split brain: both lists are monotone, each side is refused by the other when it lacks a serial, and each
  leader merges the answer and resends. The result converges to the union. The only residue is that the union is also the
  attack surface of CAF-1.
- CRL number from the clock: monotone per holder (`max(crlNum+1, now)`), so a clock that goes backwards does not lower it. A clock
  that is ahead or a leader that has not seen the highest number blocks distribution (CAF-2). It does not lose a revocation, it
  stops its delivery.
- Holder down during the revoke: the revoke task waits for an unreachable server (waitUnreachable). A standby that was down keeps an
  older sealed copy; when it becomes leader it merges the lists of the reachable servers and, if none is reachable, it sends an
  older list that every up-to-date holder refuses. No loss, but a window in which the new leader's own verifier does not know the
  serial (its list is the union of what it could read).
- Merge skipped on a path: the add-server step (membership/add.go:609-616) and the renewal push (certs.go:419) send the leader's
  list without the merge. Both only send to a server that either has nothing (new member) or refuses a stale list (409, or silently
  skipped on the pull path, cluster/pkirenew.go:93). A new member can therefore start with a list that lacks a serial for as
  long as the leader's takeover merge missed an unreachable server (CAF-9, low). Mutants N9 (sealed copy not merged in
  reconcile) and N11 (revoke task does not merge) survive, which is a test gap, not a loss, because the receiver-side refusal
  catches it.

### 2. Can a compromised or stale member block revocations or force unbounded growth?

Yes, both. See CAF-1 (any member, a revoked one included, can inject serials; the list never shrinks), CAF-3 (a member that answers
409 or any non-network error stops the revoke task for every server after it, and the revoked server is itself in the list) and
CAF-2 (a time-based number pinned in the future).

Size: /pki/install is limited to 1 MiB for the LeaderOrJoining policy (internalapi.go:563-566). The request carries every serial
twice (list and CRL). About 10 000 serials are enough for every later distribution to fail with 413 on every holder. Because a
list never shrinks, the cluster is then permanently unable to distribute a revocation. This is reachable through CAF-1.

### 3. Is the epoch fencing sound?

Against a former leader that knows it lost: yes. The new leader takes max(own file, system database, held copy) + 1 (escrow.go:337),
the recipient refuses an older blob and an older drop (N14, N16, N24 killed), and a leader that finds a higher held epoch seals
nothing (N17 killed). Across a restart: the epoch is in three places (state.json, the mirror, the held copy) and the maximum is taken,
so losing one does not lower it. Weak points:

- Epoch is not a term of a lease or a timeline, only a counter taken at adoption. Two servers that take over without having seen
  each other's value (mirror lag, no copy on the second one) both get E+1 and then race on seq (CAF-4).
- The offer does not check the epoch it receives, although wire.go says it does (CAF-5).
- A rotation on a stale leader raises the epoch above the true leader's, and the true leader then refuses to seal until its own
  operator rotates (CAF-4). The effect is availability of the escrow, not confidentiality or loss of a revocation.
- AdoptEscrow raises the epoch and generation every time it is called, which is once per B8, so a retried B8 makes the leader
  resend every copy (harmless churn).

### 4. Does the cluster_setting mirror open a new path for a database role?

No new path for an ordinary role. kingres_cp.cluster_setting is created without grants (0001_init.sql:38), written only by the
service role (ops/settings.go sets only the keys of an allow-list, so caEscrow cannot be set through updateClusterSettings), and a
superuser can already read ca.key from disk. What a writer of the row could do is listed in CAF-6 so that a future grant does not
widen it silently: switch escrow on against the operator, add holder ids that can never be dropped (an unbounded retry list, each
try up to 20 s while the escrow lock is held) and set an epoch near the 64-bit limit (overflow). The value is not returned by
getClusterSettings (the switch in pgstore/databases.go ignores unknown keys).

### 5. Is the durable write correct?

Yes on Linux. WriteFile (pki/files.go:66) creates the temp file in the same directory (mode 0600), writes, fsyncs, closes, chmods,
renames and fsyncs the directory; a failed directory sync is an error on Linux. revoked.json is written before ca.key, so a crash
between them leaves no key and the next adoption starts again. Two small points (CAF-8): the chmod follows the fsync (matters for
the 0644 files, not for ca.key, which is created 0600), and a crash between CreateTemp and rename leaves a file named `.pki-*` that
holds the CA key and is never removed.

### 6. Are the new tests meaningful?

Mostly yes. See the table of mutants. The gaps are: the revoke task merging (N11), the sealed-copy merge of a returning leader
(N9), the CRL number learned from the own file (N30), the pull path skipping a stale list (N31), and the held-epoch term of the
adoption (N19). No test uses a hostile peer for GET /revoked, which is the CAF-1 case.

## New findings

### CAF-1 (high) A server can make the leader revoke any certificate, permanently
membership/escrow.go:245-251 (`add(id, out.Serials)` for every peer answer), pki/pki.go:529 (MergeRevoked accepts any hex string),
membership/escrow.go:717 (revocationPeers includes revoked and leaving-but-active members), internalapi.go:79 and :574
(GET /revoked is an AnyPeer call).

Scenario: server S is compromised and is being revoked, or is simply buggy. The revoke task sends the list to every active server,
the revoked one included, and first calls reconcileRevocations, which asks S (unchecked, so a revoked certificate is no obstacle)
for its list. S answers with the serial of the leader's own certificate, or of every member. MergeRevoked adds them, the CA saves
them, the next CRL and /pki/install carry them to every server, and every server then refuses the leader and the other members in
mutual TLS. The new rule that a list never shrinks (CheckNotStale) makes the revocations permanent: there is no way to take a
serial out except editing files on every server. Before this change a peer's answer was never merged. The same path lets one
server inflate the list past the 1 MiB request limit (question 2). The test TestADistributionAsksTheServersFirst shows the merge
of a serial ("bb02") that the leader never issued; no test shows a refusal.

Fix: merge a serial from a peer only if it is a serial this CA issued (the leader has the issued-certificate table, kingres_cp
certificate rows) or if it is one the leader's own earlier lists contained; ignore the answer of a server whose certificate is
revoked or that is being removed; cap the number of serials taken from one answer; keep the list size below the request limit
(raise the limit for /pki/install, or send a delta). Optionally make revocation of a single serial reversible by an explicit
operator call that is signed and logged.

### CAF-2 (medium) A CRL number from the clock can pin a holder out of the cluster
pki/pki.go:490 (`max(crlNum+1, now.Unix())`), pki/files.go:321 (the receiver refuses a lower number), membership/escrow.go:229-233
(the leader learns a number only from its own crl.pem).

Scenario: holder A has a clock ahead by d seconds, or A's distribution is the last one before it dies. Its CRL number N is stored
as crl.pem on every server it reached. A new leader B that missed that install (it was down) has a lower crlNum. Every
distribution from B carries a number below N and is refused with stale_revocations by every server that has N. The repair
(reconcile then resend) merges serials but never raises the number, because the number is read only from B's own crl.pem and GET
/revoked returns serials only. B's revocation tasks fail until its clock passes N, which can be minutes (a skew) or years (a
holder with a wrong date).

Fix: return the held CRL number in GET /revoked and in the 409 answer, and call EnsureCRLNumberAbove with it before the second
attempt. Also bound the accepted jump: a number more than a day above the local clock should be logged and not pinned.

### CAF-3 (medium) One refusing server stops the revocation for every server after it
membership/certs.go:469-476 (stepCertRevokeDistribute returns on the first error), membership/escrow.go:257-274 (a second 409
is returned as the error), certs.go:450 comment ("the revoked one included").

Scenario: the server being revoked answers 409 stale_revocations (or 500) to /pki/install, whether because it is hostile or because
of CAF-2. The task fails at that server; servers that come after it in the table order never receive the list, and the task
fails again on retry. The serial is in the CA of the leader but not enforced anywhere else. waitUnreachable covers only network
errors. distributeRevocations (add.go:845) continues after an error and is not affected.

Fix: send to the revoked server last and ignore its error, and in the loop collect errors and go on, failing the task only
at the end.

### CAF-4 (low) Equal epochs are possible, and a rotation on a stale leader raises the epoch above the real leader
membership/escrow.go:337 and :617-625.

Two takeovers that do not see each other's epoch (mirror not replicated, the second server holds no copy of the first's) both
use E+1 and then compete on seq, a counter that restarts at 0 for each. A leader that is told to rotate and finds a higher held
epoch sets its epoch to held+1 (:621) and, from that moment, the true leader's blobs are refused and the true leader refuses to
seal (:617). Only the operator's rotation on the true leader repairs it.

Fix: derive the epoch from something the map group already orders (the PostgreSQL timeline id of the promotion, or a counter
written in the system database in the same transaction that makes the leader). Do not let a rotation raise the epoch above a
recipient's; make it fail with the same message as a normal round.

### CAF-5 (low) The offer ignores the epoch, against the comment in wire.go
membership/escrow.go:665-676 (the offer handler never calls `stale`), wire/wire.go (EscrowOfferRequest says a rotation from an older
leader is refused).

A stale leader can make a recipient create a new escrow key (rotate). The old key stays until a copy is stored, so nothing is lost,
but keys pile up and the doc is wrong.

Fix: call stale(req.Epoch) in the offer when rotate is set, or correct the comment.

### CAF-6 (low) The mirror is trusted more than it can be
membership/escrow.go:306-320 (`st.Enabled = sh.Enabled`, Holders added with an empty record, Epoch taken as is), :122-136
(a failed mirror write is only logged).

(a) The mirror is read from the new leader's copy of an asynchronously replicated database, so a switch-off made just before the
failure can still read as enabled; the new leader then seals the CA key to the standbys against the operator's choice until the
next sweep. (b) The mirror value is unvalidated: an epoch near MaxInt64 overflows on `st.Epoch++`, a holder list with ids that are
not members creates records that can never be dropped (a drop to an unknown id fails every round, each with a 20 s timeout under
the escrow lock). A writer needs the service role or a superuser today, so this is a note for later grants. (c) `HeldEpoch` of a
recipient is also unvalidated (:621).

Fix: validate on read (id is an active or former member, epoch below a sane bound, clamp to 2^62), treat Enabled=false as sticky
when either the file or the mirror says so, and give a record that cannot be dropped for N rounds a distinct state.

### CAF-7 (low) A record that cannot be dropped stays and is retried forever
membership/escrow.go:398-420 and :577-600.

The fix of CAE-2 keeps the record until the drop succeeds. For a server that was removed and will never answer (or an id from CAF-6)
the record, and one 20 s attempt per round, stays for ever and delays the round that holds the escrow lock.

Fix: attempt dropPending records with a short timeout and in parallel, and let the operator clear a record of a server that is
gone (with the audit entry that the copy there is still to be considered known).

### CAF-8 (low) Leftovers of the durable write
pki/files.go:66-90, pki/escrow.go:509.

A crash between CreateTemp and rename leaves `.pki-*` holding the CA key (0600, not removed at the next start). The chmod after
the fsync is not flushed by the directory sync, so on a crash a 0644 file can come back 0600. `ca.key.unusable-<time>` files
accumulate and are never read or removed.

Fix: remove `.pki-*` older than a few minutes at start; chmod before the fsync; keep at most a few `.unusable` files.

### CAF-9 (low) A new member can start with an older list
membership/add.go:609-616, membership/certs.go:419.

The add-server step and the renewal push send the leader's list without the merge of the other servers. The receiving side
protects existing members (409), but a brand new member holds nothing to compare with and accepts a list that lacks a serial the
takeover merge could not learn. The next distribution corrects it.

Fix: call reconcileRevocations before PublishLocalCRL in stepAddPrepare and renewalAnswer, or accept this and document it.

### CAF-10 (low) Compatibility of blobs across the upgrade
pki/escrow.go:54 (escrowVersion 2 and a different AAD).

A copy made by the previous version (version 1) does not open under the new AAD. A failover between the upgrade and the first resend
finds a copy that is refused and raises the critical alert. The leader resends after adoption, but only a leader that was already on the new
version does.

Fix: resend all copies at the first round after the upgrade (the generation of the new leader already does), and say in the release notes that
a failover in that window needs the manual restore.

## Tests run

- Unit tests on Windows: internal/pki, internal/membership, internal/cluster: pass at 6e8e13ddbc.
- Cluster tests, lane l4, private install copy with the extensions of b049d04bd5, `-Run TestCAEscrow`:
  TestCAEscrowMapGroupFailover PASS (115.8 s), TestCAEscrowTamperedCopyIsNotAdopted PASS (100.1 s),
  TestCAEscrowARevocationAfterTheFailoverHoldsWhenTheOldLeaderReturns PASS (110.3 s). The private install and the worktree were removed.

## Mutants

Each applied alone to the committed source and run against ./internal/pki, ./internal/membership and ./internal/cluster
(Windows), restored after each run. The first batch also included ./cmd/kingresd (N1 to N11). A "killed" row names the first failing
tests; N8 and N20 and N27 were killed by a test that is not about the change, probably because of load; I count them as killed but not
by a directed test.

| Id | Change | Result |
|---|---|---|
| N1 | stale check ignores a lower CRL number | killed |
| N2 | stale check ignores a missing serial | killed |
| N3 | InstallPKI does not call the stale check | killed |
| N4 | InstallPKI replaces the serial list (no union) | killed |
| N5 | InstallPKI does not merge into the holder's CA | killed |
| N6 | CRL number not taken from the clock | killed |
| N7 | reconcile does not ask the peers | killed |
| N8 | reconcile skips revoked-serials.json | killed (by an unrelated test) |
| N9 | reconcile skips the sealed copy | SURVIVED (gap: returning leader, copy only) |
| N10 | distributeRevocations does not reconcile | killed |
| N11 | revoke task (stepCertRevokeDistribute) does not reconcile | SURVIVED (gap) |
| N12 | AdoptEscrow does not reconcile | killed |
| N13 | installRevocations does not retry after 409 | killed |
| N14 | Position ignores the epoch | killed |
| N15 | Position ignores seq | killed |
| N16 | drop is not epoch-fenced | killed |
| N17 | sendEscrow ignores a higher held epoch | killed |
| N18 | adoption does not raise the epoch | killed |
| N19 | adoption ignores the epoch of the held copy | SURVIVED (the file and the mirror cover it in the tests) |
| N20 | adoption ignores the epoch of the mirror | killed (unrelated test) |
| N21 | a failed drop forgets the record | killed |
| N22 | grace of one round instead of three | killed |
| N23 | Misses is not reset for a candidate | killed |
| N24 | store skips the older-than-held check | killed |
| N25 | adoption turns escrow on from a copy, not the mirror | killed |
| N26 | offer does not report the held epoch | killed |
| N27 | pki adoption ignores the CRL number of crl.pem | killed (unrelated test) |
| N28 | ca.key written with a plain os.WriteFile | killed |
| N29 | AAD without length prefix | killed (a mutant with a zero prefix survives; it keeps fixed-width separators) |
| N30 | reconcile does not raise the CRL number from its own crl.pem | SURVIVED (gap, and the number is not learned from peers at all, CAF-2) |
| N31 | pull-path install ignores a stale list | SURVIVED (gap) |
| N32 | revocationPeers includes the leader itself | SURVIVED (low value, the test seam skips the call) |

Not covered by any test or mutant: a hostile answer of GET /revoked (CAF-1), a 409 that persists (CAF-3), a CRL number above the
local clock (CAF-2).

## Fixes

See the section "Fixes of the review of the fixes" at the end of [the first review](2026-11-22-ca-key-escrow.md): each finding, its fix, its test and its mutant.
