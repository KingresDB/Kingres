<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->

# Independent review of the collector and the publication of the ledger in kingresd (phase 7, steps 5 and 6) and of the fixes for the review of the signing keys

Date: 2026-11-19. Reviewer: independent, read-only on the export; all probes ran in a private copy that was removed at the end.

## 1. Scope and method

Reviewed: `internal/ledgerround` (reader, source, wire, collector, chain, extend, resign, jobs, aggregate, retire, digest, views, objects), `internal/ledgerpub` (store, pgstore, memstore, path, http), the publication part of `internal/ledgersign` (`publish.go`, `events.go`, `tasks.go` retire steps, `keyring.go`), `internal/scheduler` (term guard), migrations 0030 and 0031, the internal-call policy for `ledger/read` and `ledger/publication/get`, the hooks in `ops` (dropDatabase) and `membership/remove.go`, the wiring in `cmd/kingresd/ledger_signing.go`, design `ledger-phase7.md` (sections 4, 5, 9.4 and corrections 137 to 149), the review of the signing keys (P7-01 to P7-16) and `kingres/verifier`.

Method: reading of every file above; unit-level probes and mutants in the real packages (Windows, in-memory store); the author's integration tests and my own PostgreSQL tests in WSL on PostgreSQL 18.7 and 19rc2 (real instances, a real two-server, two-group cluster for the cluster tests); the standalone verifier run against a publication written by a real two-group cluster, then against copies of it changed by one byte.

What I did NOT do: no `ALTER EXTENSION UPDATE` run, no real failover or physical restore on a cluster, no fuzzing, no TLS handshake tests beyond reading, no PostgreSQL-level mutants of `pgstore.go` (only in-memory mutants and one real-database interleaving test).

## 2. What was run

| Run | Major | Result |
|---|---|---|
| `go test` of `ledgerround`, `ledgerpub`, `ledgersign`, `scheduler` (Windows, in-memory) | n/a | all pass (4 packages) |
| Author tests with PostgreSQL: `TestTheDatabaseStoreKeepsTheRulesOfTheStore`, `TestTheDatabaseRefusesAnUpdateOfATile`, `TestAKeyChangeThatReadTheRowBeforeTheFirstCheckpointDoesNotUnlockThePrefix`, `TestADeadlockIsBusyAndNeverAConflict`, `TestRoundsCutAtEveryWriteOnTheDatabaseStore`, `TestDivergenceAndRetirementOnTheDatabaseStore`, the `ledgersign` database-store tests (key lifecycle, deposed writer, two changes, kills) | 18.7 | 14 tests, all pass (the same run also matched my in-memory `TestP8DropClearsDivergence`, which passed) |
| Cluster tests `TestLedgerPublicationOfADatabaseOnTwoGroups`, `TestLedgerRoundSurvivesAKillAtEachKindOfWrite`, `TestLedgerPublicationEndsWhenADatabaseIsDroppedAndWhenItVanishes`, `TestLedgerPublicationStopsWhenAGroupLosesItsTail`, `TestLedgerSigningKeysAreMade...AcrossTwoServers` | 18.7 | 5 of 5 pass (18 s, 39 s, 27 s, 17 s, 29 s) |
| My PostgreSQL tests (`TestP8RetirementAndSigningExcludeEachOtherOnTheDatabase`, `TestP8ReplaceNoteChecksNothingOnTheDatabase`) and the publication test again with a clean fixture directory | 18.7 | pass (the first shows the exclusion holds; the second shows P8-06) |
| Same author set plus my tests | 19rc2 | 28 tests in 4 packages, all pass (the author's database-store, kill, divergence and deposed-writer tests, the 5 cluster tests, and my probes; the code is identical on both majors, no version difference showed) |
| My in-memory probes (`TestP8SystemRestoreForksTheAggregate`, `TestP8VanishedRuleCountsRoundsNotTime`, `TestP8SwappedAnswers`, `TestP8DropClearsDivergence`, `TestP8RateTableFull`, `TestP8PathTricks`, `TestP8RetirementLivelocksWhenTheOtherKeyKeepsSigning`) | n/a | run; their output is quoted in the findings |
| Mutants of my own (one-line): 12 on the collector (one of them equivalent), 16 on the store, 12 on the endpoint and the path parser | n/a | see section 7 |
| `kingres-verify checkpoint` on a real publication (2 chains, 1 aggregate) and on 8 altered copies | 18.7 | see section 8 |

## 3. Fate of the findings P7-01 to P7-16

| Finding | Fate | Evidence |
|---|---|---|
| P7-01 signing and retiring exclude each other | The exclusion holds on a real database, but the fix introduced a permanent failure of every retirement that sees any later checkpoint (P8-02). | PostgreSQL test: a checkpoint write that holds the guard (`FOR SHARE` on the cluster row) and a key retirement with a bound started meanwhile: the retirement waits, and when the write commits it is refused ("a checkpoint above the bound of the retired key was stored meanwhile"). Read: `Term.Guard` takes `FOR SHARE`, the key `Commit` raises the revision (needs the row exclusively) and checks the bound after the update, `PutCheckpoint` reads the revision after the guard. Both orders are serialised. `Publication` skips a key whose retired event exists. |
| P7-02 clock second source | Not changed (no mark in code, README says nothing new). Not re-tested. | read |
| P7-03 forgotten holder | Fixed in the main (a removed, unreachable server is dropped from the holders record with a log line and a task detail). Residual: no alert and no state in the API; the key files remain on that host and nothing reminds the operator after the task ends; a returning server is not cleaned unless it becomes a group-0 holder again. A returning server cannot sign for a retired or revoked key (the set says so) and does not serve key files (`ledger/key/*` are leader-only). | read `tasks.go` `removeStale`; the author's test passes |
| P7-04 unopenable key file | Fixed: `Keyring.Verify` opens the file and compares the public key, the alarm and the distribution use it. | author's test passes (Windows) |
| P7-05 events used unverified | Partly fixed. `verifyEvents` checks numbering, that each note parses, that number and type match the row, and that at least one signature is by some key of the stored set. It accepts an event signed by a key that only the key rows say is in the set (a writer who adds a row for his own key signs his own events), does not check that a key was authorised by earlier events or valid at the event's time, and accepts events deleted from the end. The README says so. Residual severity: low. | read `events.go` line 115 |
| P7-06 compromised-since | Fixed (decided once in the event, read back by the revoke step). | read |
| P7-07 seed text in strings | Not changed, documented. | read |
| P7-08 path in an error | Fixed (`Verify` and `noPath`). | read |
| P7-09 temporary key file | Fixed (files `.key-*` older than a minute are removed). | read |
| P7-10 adoption marker | Not as claimed (P8-07): the marker is a plain `<fingerprint>.task` file, its content is only checked to be a number, the task id is never compared with the task that adopts, and anyone who can write the directory writes it. Low, as before. | read `keyring.go` lines 344 and 455; `Orphan` has no task argument |
| P7-11 stale next keys and task loops | Partly fixed (a retirement is not restarted at once). The loop reappears in a worse form through P8-02. | read, test |
| P7-12 key files outside the guard | Fixed in the main (the set is read again before each removal). | read |
| P7-13 leader change between generate and install | No change seen. | read |
| P7-14, P7-15, P7-16 | No change seen; not re-tested. | read |

## 4. Duties of the caller of `ledgerlog`

| Duty | Fulfilled? | Evidence |
|---|---|---|
| One writer per log | Yes for one leader: one mutex per database in `Collector` and the term guard on every write of `PGStore`. Across a restore of the system database: no (P8-01). | `collector.go` `lock`; `pgstore.go` `write` |
| Order of a round (Append, frontier with size and root, sign, PutCheckpoint) | Yes. | `extend.go`; kills at tile, frontier, checkpoint, pending pass on a real cluster |
| Open with the stored frontier, size, root | Yes (`openStored`). | `extend.go` line 27 |
| Append once per checkpoint | Yes (one `Append` per round per log; the aggregate appends the pending leaf and then the new one, each followed by a checkpoint). | `aggregate.go` |
| Crash repair with `AllowTilesAhead` only after `ErrTilesAhead`, same leaves | Yes. | `openStored` |
| On `ErrConflict` stop the log, never repair | Yes for tiles and for a store refusal of a checkpoint (`divergeFatal`). A conflict of `SaveFrontier` is returned as a round error and does not stop the log (correct for a stale writer). | `extend.go` |
| Sign only `Log.Checkpoint()`, never smaller, never two roots | Yes inside one store lifetime (the check `cp.Size == l.Size` and the store rules). Not across a restore of the system database for the aggregate log (P8-01). | `ensureCheckpoint` |
| Give `SignCheckpoint` every signer of `Set.Signers` | Deliberately not: a key whose retirement was begun is left out (P7-01). Auditors that pin only that key cannot verify checkpoints made after the event, which is acceptable only while the new key is in their trust file. Note. | `publish.go` |
| Re-sign the latest checkpoint of every log at a rotation | Yes (`Resign`, all logs, including closed and diverged). `ReplaceNote` accepts any note (P8-06) and has no revision check. | `resign.go` |
| Run Audit from time to time | Not built on purpose (excluded). | n/a |
| Keep every partial tile | Yes, no pruning. | n/a |
| Reader per request | Yes (`lastLeaf`, `leaf` create one and drop it). | `aggregate.go`, `views.go` |
| Context honoured | Yes. | read |

## 5. New findings

### P8-01 (critical): a restore of the system database from an older backup makes the publisher sign a second root for an aggregate size, and a second event for a sequence number, with no alert

Place: `internal/ledgerround/aggregate.go` lines 79 to 160 (the aggregate leaf holds `GeneratedAtUS` read from the clock and the members as found now), `internal/ledgerround/retire.go` and `internal/ledgersign/publish.go` `AppendEvent` (events hold a time), design 9.4 ("Restoring only the system database": "the same leaves give the same bytes ... re-signed checkpoints are byte-identical").

The claim of 9.4 holds for a chain log (its leaves are the blocks) and does not hold for the aggregate log or for the events: their leaves and notes depend on the clock and on what the chains looked like at that moment.

Steps (in-memory store, a fake chain; `TestP8SystemRestoreForksTheAggregate`): a live collector publishes aggregate leaves 0, 1, 2 (checkpoint sizes 1, 2, 3). A second store, the "restored" one, holds the same history up to an earlier point (aggregate size 1). The chain has moved on. The restored store is given to a collector and rounds run.

Output:

    live aggregate size 3, restored aggregate size 3
    size 2 live root ebe5407f restored root 3842d87f same=false
    size 3 live root 107872ec restored root f62900d8 same=false
    (no alert raised)

Both stores sign a checkpoint for aggregate size 2 and size 3 with different roots, under the same key, and nothing in the collector can notice, because everything it compares with is in the restored database. A mirror or an auditor that holds the earlier checkpoint sees a fork of the aggregate that the publisher itself signed; the divergence alert, the thing the design relies on ("the publisher is itself a monitor"), does not fire. The same happens to the events: after a restore the next `chain-retired` or key event is written with a sequence number a published event already has, with other text (and the key rows, the trust file and the state of the retired and revoked keys go back too). Case also met after a restore: a log that was `diverged` or `closed` after the backup becomes `active` again.

The only documented mitigation is the restart of every `kingresd`, which deals with the term and with nothing else here. This is a sequence an operator meets (disaster recovery of `kingres_cp` alone is the documented procedure of 9.4).

A fix must achieve: the publisher must not be able to sign a size that a published checkpoint already covers with different content. For example the leader keeps (outside the system database, in a file next to the key files, sealed with the keys, or in the key directory's own state) the highest published size and root of every log and refuses to sign when the database is behind it, raising a critical alert "the system database is older than what was published"; and aggregate leaves and events must either be reproducible from the chain logs (time taken from the newest chain block, not the clock) or be recoverable from the mirrors. The design text of 9.4 must say which of these holds.

### P8-02 (high): after the retired event, any checkpoint of any log makes the retirement of the key fail for good; the key stays active, excluded from signing, and the task loops

Place: `internal/ledgersign/tasks.go` `stepRetire` line 510 (`if max > size`), `pgstore.go` `Commit` (`CheckpointBound` against `max(size)` of all checkpoints), `stepEventRetired` line 436.

The fix of P7-01 compares the largest stored checkpoint of the whole cluster with the bound decided in the first step. After the event exists the retiring key is not offered for signing any more (`Publication`), but the other key keeps signing and the logs keep growing, so the largest stored size passes the bound as soon as one new checkpoint is stored by the new key. The check does not ask who signed. The bound is decided once (a repeated step finds the event and returns), so no later attempt can succeed.

Steps (`TestP8RetirementLivelocksWhenTheOtherKeyKeepsSigning`, in-memory store): create two keys, advance the clock, start the retirement, raise the largest stored size from 100 to 101 after the event (a checkpoint signed by the new key only).

Output:

    first task: failed {Code:bound_exceeded ... a checkpoint of size 101 was stored after the bound 100 ... Retryable:false}
    retry 0: failed {... size 102 ... bound 100}
    retry 2: failed {... size 104 ... bound 100}
    state of the key: active
    with no further growth: failed ... key active

A new retire task is accepted by the API and fails again each time, also when growth has stopped. The key is in the state "excluded from signing, never retired" for ever: the key set cannot finish a rotation, the key keeps an unbounded validity in the trust file (a later compromise of that key lets the holder sign any size for the old key id), and the due job starts a task row every few minutes. In a cluster whose logs grow every round the window is the time between two engine steps; in a quiet cluster it passes, which is why the author's cluster test passes.

A fix must achieve: the refusal is about checkpoints signed by this key above the bound (read the note lines or record the signer set per checkpoint), not about the largest stored size; or the retire step recomputes the bound from the checkpoints stored before the event and the event is only a notice; a test in which the other key keeps signing between the two steps and the retirement still ends.

### P8-03 (high): the "database vanished" rule counts rounds, not time, and a retirement is irreversible; a healthy database whose groups answered "no database" twice is closed for ever, silently

Place: `internal/ledgerround/collector.go` `noteMissing` (line 305), `jobs.go` lines 55 to 80 (`MissingRounds = 2`), `chain.go` line 86 (`hasClosed`), `retire.go` `RetireDatabase`, design 148.

Every call of `round` counts, including the rounds of a REST digest (`Digest`), of `finalRound` and of the job. The rule is "every group answered `no_database` in two rounds in a row", and nothing consults `DatabaseExists` (configured, used for the 404 only) or the catalog. Steps (`TestP8VanishedRuleCountsRoundsNotTime`): publish a chain; make every group answer `no_database`; one `Digest` call and one job pass a second apart.

Output:

    after 2 missing rounds: .../c/0102.../e1 kind=chain state=closed detail="retired: database-missing"
    (the database is back, same chain id, 3 more blocks)
    after return: group 0 log=false diverged=false err=<nil>
    final: .../c/0102.../e1 kind=chain state=closed size=4
    alerts: no ledgerChainDiverged raised, no unread alert

The chain is closed with a signed `chain-retired` event, a new round finds `hasClosed(chainID)` and returns nothing: no log, no alert, the new blocks are never published, and there is no step that reopens it (the acknowledgement and the new epoch are step 20). The same silent end follows the `dropDatabase` hook when the drop task later fails or is cancelled (the retirement is the first step and is not undone), and, from reading only (not run), a group that is removed and added back with its data intact (`RetireServer`, reason `group-removed`, then `hasClosed` again).

Realistic precondition: a database restored or rebuilt (physical restore, a rebuilt primary, a move of the data directory) while its groups answer 3D000 for two rounds (the interval is 10 to 3600 seconds, default 60). That is the case the question asked about.

A fix must achieve: a time threshold (for example the group answers "no database" for a configured number of minutes, with the database absent from `pg_database` of the system instance too), a check that the name is no longer in the catalog of the control plane, an alert before the retirement, and a chain log that was closed for `database-missing` must be reopened (new epoch with an event) when the same chain id comes back instead of being ignored in silence.

### P8-04 (medium): an answer with another chain id, from a swap of two groups or from a forged answer, retires both logs, publishes two signed events and raises no alert

Place: `internal/ledgerround/chain.go` lines 60 to 100, `retire.go` `retireLogs` (clears alerts, raises none).

A group whose answer names another chain than the published one gets `chain-replaced` (design 141). When two groups swap their answers (a mapping error, or a server that holds the internal certificate answering for another group) the first group closes its log and creates a log for the other chain, whose origin is the one of the other group's open log (`CreateLog` returns the existing row), and the second group then closes that very row. `TestP8SwappedAnswers` output:

    group 0 ... state=closed detail="retired: chain-replaced"
    group 1 ... state=closed detail="retired: chain-replaced"
    events: chain-retired ... reason=chain-replaced (two)
    alerts: only clears

Both healthy chains end for good (`hasClosed`), with signed events that say "replaced", and no alert at all. The same holds for any single forged answer: a holder of the internal certificate can end the publication of any chain it names a different id for, and can start a log for a chain of its own. A rollback or fork of the same chain id is correctly detected (section 6).

A fix must achieve: a `chain-replaced` outcome raises a critical alert (the event alone is not a signal); an answer whose chain id is the id of another group's open log is a divergence of both, not a retirement; and the new-chain log is created only when the old one retired without a conflict of origins.

### P8-05 (medium): the per-address rate table can be kept full so that new auditors get 429

Place: `internal/ledgerpub/http.go` lines 79 to 92.

When the table holds 4096 addresses and none is older than 10 seconds, every address that is not in the table is refused (`return false`). Probe (`TestP8RateTableFull`): 4096 requests from distinct IPv6 addresses (one each), then a new auditor.

Output:

    a new auditor address, 5 s later: 429
    a new auditor address, 11 s later (purge): 200
    another auditor after refill: 429

About 400 requests per second from a /64 (or a small botnet) keep every new address out of the publication for as long as the attack lasts; addresses already in the table are served. The endpoint is the thing auditors use to catch a rewrite.

A fix must achieve: a full table evicts the oldest entries (or buckets by network prefix, /64 for IPv6) instead of refusing the newcomer; a test that fills the table and then serves a new address.

### P8-06 (medium): `ReplaceNote` stores any note

Place: `internal/ledgerpub/pgstore.go` line 308, `memstore.go` line 290; the `Store` contract says the note adds signature lines to the same text and the root must equal the stored root.

Test on PostgreSQL 18.7 (`TestP8ReplaceNoteChecksNothingOnTheDatabase`): after a checkpoint is stored, `ReplaceNote` with "completely different text\n\n- sig\n" returns nil and the served checkpoint is that text. Only the collector calls it, and it uses `ResignCheckpoint`, so no wrong note comes out today; but the store, which is the barrier the design describes ("never a second root for a size"), does not hold this rule, and the in-memory store the tests use is as permissive, so no test could tell. Also `Resign` has no revision check: a re-signing that loaded the signers before a key event can add a signature of a key that was retired or revoked meanwhile (low effect, the sizes are inside the bound).

A fix must achieve: `ReplaceNote` parses the note and refuses one whose text (origin, size, root) is not the stored checkpoint's, and checks the key revision as `PutCheckpoint` does; a test for both on both stores.

### P8-07 (low): the adoption marker of P7-10 is not tied to a task

Place: `internal/ledgersign/keyring.go` `Orphan` (line 344), `marked` (line 455). The fix says a file is adopted only when it carries the marker of the task that asks; `Orphan` takes no task id and `marked` only checks that the marker parses as a number. Anyone who can write the key directory (the same person who could write the key file) writes both. The property of the review ("only the daemon can write it, limited to the task") does not hold; the extra capability for an attacker is as small as before. A fix must achieve: the marker holds the task id and a keyed hash (or the sealed content names the task), and `Orphan` takes the id.

### P8-08 (low): dropping a database or removing a group clears a divergence

Place: `internal/ledgerround/retire.go` lines 36 to 56. `RetireDatabase` and `RetireServer` retire diverged logs too: the log is closed with the event "final head = the published head", the critical alert is cleared (`TestP8DropClearsDivergence`: state closed, "retired: dropped", alert cleared, one event with the old head), and the last aggregate leaf flags the member `retired`, not `diverged`. An administrator who rolled a database back can end the signal by dropping it; the earlier leaves still carry the `diverged` flag, so an auditor can see it, and the drop is destructive anyway. Note for the acknowledgement of step 20: a diverged member should stay flagged diverged in the retiring leaf.

### P8-09 (low, tests): mutants that survive and kills that do not reach every write

In-memory mutants of the collector (section 7): survived `headLen < published-1` (a tail loss of exactly one block is untested), removal of the "empty answer after blocks were published" divergence (untested), removal of both divergence checks of `ensureCheckpoint` (they are unreachable behind the store's own refusal; defence in depth with no test), removal of the check that the signed tree is the stored one. Endpoint: the table cap of 4096 is untested (it is the DoS of P8-05), `nosniff` and the CORS header are untested, and the concurrency limit is "killed" only by a test timeout. The cluster kill test cuts the writes `tile`, `frontier`, `checkpoint`, `pending` only; `log`, `state`, `retired`, `unread`, `note`, `digest` and the key events written by `AppendEvent` have no kill at the process level (the in-process test on the database store cuts by write number). The kill test also cuts before a write, never after the commit; the idempotent design makes that equivalent, but nothing shows it.

### P8-10 (note): smaller observations

- `events/<seq>` is served `immutable` for a year; after a restore (P8-01) a mirror that cached an event keeps it. Same for any tile.
- The aggregate id is a hash of the cluster id and the database name; the design calls the cluster id public, so a guessable database name can be tested against the id. 4.9 says "hidden"; it is hidden only as far as the name is unguessable.
- The TLS certificate is read once at start; a renewed certificate needs a restart. Minimum version TLS 1.2; HTTP/1.1 only.
- A forged peer can make the leader read up to 64 MiB of answer (`peerclient`), and `ReadResponse.Entries` is not compared with the asked `Max` before `Append` (which refuses more than 10 000). Bounded; the memory is a few hundred MiB at worst, inside the 2 GB rule, but not "bounded by the call".
- A new hash version of blocks (a later ledger version) would be seen as "not a block header" and stop every log for ever (141: a diverged log never wakes up). Needs a decision before a hash version 2 exists.
- `Files.Objects` on a follower falls back to the leader's call; a deposed leader still serves from the system database until the next leader lookup (reads only). A checkpoint and its tiles are read in separate transactions; because tiles are stored before the checkpoint a reader sees tiles ahead of the checkpoint, never the reverse, so an honest verifier is not shown an inconsistent state.

## 6. What held under attack

Detection (in-memory harness with a real tile log, plus the cluster test "group loses its tail" on PostgreSQL 18.7):

| Change under the publisher | Detected | When | Signed meanwhile | Log continues? |
|---|---|---|---|---|
| tail lost (older backup, failover loss) | yes, head below published size | next round (interval, default 60 s; also any digest request) | nothing | no |
| block rewritten at the head, just below, or deep (chain relinked) | yes: the block hash covers the previous hash, so any rewrite below the head changes the block published-1 or the head, both compared every round | next round | nothing | no |
| same chain id, different history after a rollback and regrowth | yes (block published-1 differs) | next round | nothing | no |
| chain replaced by another id | yes, `chain-replaced` (not a divergence), see P8-04 | next round | event only | a new log |
| database dropped and re-created | new id: `chain-replaced`; if the new ledger is still empty: `diverged` first, then `chain-replaced` when the new chain appears | next round | event only | a new log |
| a group's answer forged with the same chain id | detected only if it contradicts the published history | next round | nothing | no |
| two groups swap | P8-04 | next round | two events | no |

The author's statement that only the head and the block before it are checked per round is correct and sufficient for rewrites of block headers, because of the hash chain. What it leaves to the audit of step 20 is everything under the block: rows changed without a block rewritten (the transaction Merkle root of a block is not recomputed), and a group that lies consistently from the start. The published data lets an outside auditor detect a rewrite of the chain as a fork between two checkpoints (consistency proof from the tiles), which the verifier does (section 8).

Term guard and revision: every write of `PGStore` goes through `write` (guard first); the in-memory mutants that remove the guard of each of the ten write kinds all die; `PutCheckpoint` refuses another root, a smaller tree, a tree that is not the stored one, a log that is not active, and a changed key revision; the real-database interleaving test shows the exclusion with retirement (P7-01). No path I found stores a checkpoint for a smaller size or another root inside one store lifetime, including the repair after a crash at each of the four writes (cluster kill test passes), the re-sign (`ReplaceNote` never changes size or root in the collector), a leader change (the new term is taken after in-flight writes finish) and two leaders (the old one gets `ErrDeposed`). The cases that break this are the restores of P8-01.

False divergence: failover without data loss, a standby promoted, bucket moves (no ledger row moves, design 5.1), `ledger_enable`, an old extension (`ledger_too_old`, unread, not divergence), a group that answers slowly or in part (unread), a deadlock or a serialisation failure (`ErrBusy`, never a stop; test passes), a key rotation in the middle of a round (`ErrKeysChanged`, next round signs again) all end as "unread" or "try again", not as divergence. I found no ordinary sequence that stops a healthy log, apart from the permanent closing of P8-03 and P8-04.

Endpoint: 15 hostile request forms (double slash, dot, encoded characters, semicolon, query, fragment, absolute URI, upper case, trailing slash, header injection in the path, `Range`) gave 404, 400, or the one file named; only `GET` and `HEAD` are served (`OPTIONS` 405); no redirect; `Range` is ignored and a full 200 is sent; immutable and mutable cache headers are right, misses are cached 5 s; nothing besides the layout of 4.6 is served (the key ring is the public trust file).

Privacy: tiles carry the 87-byte block header; the checkpoint and the aggregate carry hashes, sizes, chain ids, flags and times; events carry chain id, epoch, final height and hash, reason; alerts and logs carry database names but are not published; error texts of the reader are the extension's messages (block numbers) or fixed text.

`ledger/read`: the internal policy is `LeaderOnly`; the body limit is 1 MiB; the database name is checked as an identifier; no SQL text; `Max` is bounded to 10 000 (about 1.2 MB of answer); a backlog above 9 999 blocks is caught up in several rounds and each round checks the block before the batch.

## 7. Test quality: mutants of my own

Collector (in-memory, author's suite plus nothing of mine): killed: head hash compare at equal height, first-block compare, `CheckLink`, chain-replaced branch, closed-log skip, vanished rule (all groups, two rounds); survived: M1 (`headLen < published-1`), M6 (empty answer after published blocks), the two divergence checks of `ensureCheckpoint`, the signed-tree equality check (these last three are defence in depth).

Store (in-memory): ten write guards, revision check, "not the stored tree", frontier shrink and state checks: all killed. "A smaller tree than one signed" and "another root for the size" survive in the in-memory store because the earlier check makes them unreachable (equivalent mutants); the same checks in `pgstore.go` were not mutated.

Endpoint: killed: method, raw-path, rate limit, cache header, TLS minimum version, body on HEAD, miss caching; survived: table cap, `nosniff`, CORS header; concurrency limit killed by timeout only.

Timing: the cluster tests wait with `until` loops (up to minutes) and I saw no verdict that depends on a fixed sleep; the divergence tests use an injected clock.

## 8. The standalone verifier

A publication of the author's two-group test (2 chain logs of size 3 and 1 aggregate of size 2) was verified with `kingres-verify checkpoint`:

| Case | Result |
|---|---|
| untouched, 2 chains and the aggregate | `VERIFIED`, exit 0 |
| one byte of each chain checkpoint changed | exit 1 `parse` (control character) |
| one signature byte changed | exit 1 `signature` |
| the size line changed to 9999 | exit 1 `signature` |
| trust file: public key character changed | exit 2, fingerprint mismatch named |
| trust file: key marked revoked | exit 2, no key can be used |
| `--min-size` above the size (a roll back below what the auditor knows) | exit 1 `min-size` |
| `--state` from the genuine checkpoint, then a tile (entries bundle) changed | `VERIFIED` (same size: no tile is read; documented) |
| tile changed, no state | `VERIFIED` (documented: `checkpoint` reads no tile) |
| a state file made for another log | refused, file kept, instructions printed |

Not run: a roll back to an older genuine checkpoint (the fixture keeps only the latest checkpoint of each log), a changed event (the verifier has no command that reads events or the aggregate leaves), `consistency` with two sizes. The verifier accepts what is published and refuses what I changed; it does not yet check events, `chain-retired` against the aggregate, or entry bundles.

## 9. Verdict

Anchoring and the verifier's next steps may be built on the chain logs: the detection of rewrites, rollbacks and forks of a chain is complete for block headers, the store rules are enforced in one place, and the serving is tight. They must not be built on the aggregate log, the events and the retirement logic as they are.

Fix first:

1. P8-01 (critical): a restore of the system database forks the aggregate and the events and nothing notices. An anchor on the aggregate would anchor whichever fork the publisher signs last.
2. P8-02 (high): a key retirement cannot finish while the logs grow; the key rotation of an anchoring run depends on it.
3. P8-03 (high) and P8-04 (medium): irreversible, silent closing of healthy chains (rounds counted instead of time, no alert for `chain-replaced`, no reopening).
4. P8-05, P8-06 (medium): the endpoint can be kept closed to new auditors; the store does not enforce its own contract for notes.
5. Then the low items and the test gaps of P8-09.

## 10. What was not checked

The daily audit, the acknowledgement and new epochs, anchoring, pruning of partial tiles, `LedgerStatus.publication`, rounds one after another (excluded by the task). In addition: a real failover or physical restore on a cluster (the sequences were run in the in-memory harness with a fake source; the cluster tests run the tail loss, the drop, the vanish and the kills); `ALTER EXTENSION UPDATE` during a round; the removal-of-a-server sequence beyond reading `membership/remove.go` (whether a group's primary can move off the server before `RetireServer` runs); PostgreSQL-level mutants of `pgstore.go`; TLS on a real handshake; the in-process cache of a follower under load; P7-02, P7-07, P7-13 to P7-16 beyond reading.
