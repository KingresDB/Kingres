<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->

# Independent review of the fixes for the collector and publication review (P8-01 to P8-10) and of the wiring of anchoring into kingresd (phase 7, steps 10, 12 and 14)

## 1. Scope and method

Commit reviewed: `83efaba1265f` (head of branch `kingres-18`). Steps 17 and 20 (client keys, bundles, epochs) are on `p7-work` and were not looked at.

Part (a): the fixes for `docs/reviews/2026-11-19-kingresd-ledger-collector-and-publication.md`, as described in `kingres/README.md` (row 7) and the control-plane README (corrections 171 to 178): the record `published.json` (`internal/ledgerstate`, `internal/ledgerround/published.go`, the hooks in `extend.go`, `retire.go`, `ledgersign/service.go`), the retirement bound (`ledgersign/tasks.go`, `pgstore.go signedByMax`), the vanished-database rule (`collector.go vanished, noteMissing`), the swapped and replaced chains (`chain.go`).

Part (b): `internal/anchor` (records, supersede, backoff), `internal/ledgeranchor` (service, jobs, PG and memory stores, three kinds, credentials, egress, alerts, REST, agent), `cmd/kingresd/ledger_anchor.go`, `internal/httpapi/handlers_ledger_anchor.go`, migration 0032, and design section 6 (6.1, 6.2, 6.6 to 6.9). The libraries `anchor/worm`, `anchor/sigv4`, `anchor/tsa`, `anchor/ots` were taken as reviewed five times; I used them through the wiring and did not re-read their parsers.

Method: read the design and the code; work adversarially with tests of my own (not committed, not left in any tree); one-line mutants of the code, each in a clean copy of the tree, each followed by a build and the unit packages; no cluster test finished (section 7).

## 2. What was run

- Baseline on the head: `go build ./...`, then `go test -p 2` of `internal/ledgeranchor`, `internal/anchor/...`, `internal/ledgerstate`, `internal/ledgerround`: all pass (Windows, Go 1.27.3). The tests that need `KINGRES_PG_BIN` (the PostgreSQL store, `pg_test.go`) skip here and were not run.
- My own tests, in the packages `ledgeranchor` (memory store, the fake S3 of the package) and `ledgerround` (memory store, fake source): starvation of the jobs, a hung target, evidence accepted for another checkpoint, evidence file of a record that is not anchored, a leader without the credentials file, the text of a missing `credentialsPath`, a result of an old configuration, an old failure and the alert, a table of lying stores, forms of a secret in the redactor, a record that cannot be written, one forged answer, the same size with another root, an unreachable group that is not a vote, a damaged record.
- 48 mutants (section 6; 5 of them did not compile and were redone).
- The cluster tests `TestAFollowerThatForwardsAHostileBodyLeaksNoCredential` and `TestLedgerPublicationEndsWhenADatabaseIsDroppedAndWhenItVanishes` were queued behind the machine-wide mutex through `scripts/run-integration.ps1`; the mutex stayed busy (section 7).

## 3. Findings

Counts: critical 0, high 1, medium 5, low 8, note 3.

### P7A-01 (high): the due list is cut at 100 rows before the state and the due time are looked at; claimed records starve every pending record of every target

Place: `internal/ledgeranchor/pgstore.go` `Due` (and `memstore.go` `Due`), `jobs.go` `pass`.

`Due` returns the oldest 100 rows (`ORDER BY a.id LIMIT 100`) of the states pending, submitted and claimed, and takes every claimed row whatever its next check (`OR a.state = 'claimed'`). `pass` then drops the rows that are not for this job or not due. A claimed OpenTimestamps record has a next check in the future, so it takes a place and is dropped. With 100 or more such records older than a pending one, the pass finds nothing to do, for the submit job and for the upgrade job, for every target. Without a `headerSource` an OpenTimestamps record never leaves `claimed` until `ClaimedTimeout` (days), so the precondition is "an OTS target without a header source and 100 records": 5 databases and one record an hour is 20 hours.

Steps (`TestRevStarvation`, memory store, whose `Due` has the same rule as the SQL): one enabled WORM target; 100 claimed rows with ids 1 to 100 and a next check in 6 hours; a pending record of the WORM target (id above 100). One `jobSubmit` pass. Output: `pending rows of live targets after a submit pass: 2` and the test fails; the pending records are never submitted. The SQL was read, not run (no PostgreSQL here).

Why it matters: anchoring stops for all targets, silently. The behind alert comes at `3 * interval + 1 h` (about 4 hours), the critical one at about 13 hours, and neither says why; a WORM and a TSA target that work are starved by an OTS target that merely waits.

A fix must achieve: the selection by state and due time is made in the query (pending and submitted whose `next_attempt_at` has come, claimed whose `next_attempt_at` has come or is null), the limit applies after that and per target or round robin, and a test with more than `BatchSize` records that are not due in front of a due one, on both stores.

### P7A-02 (medium): after the fix of P8-01 a real restore of the system database has no way back, and the design still describes the old one

Place: `internal/ledgerround/published.go checkPublished`, `internal/ledgerstate/state.go Replace` (used only by `buildState`), design 9.4 "Restoring only the system database" (lines near 1613), README correction 171.

A database restored from an older backup is refused for ever: `Check` wants a checkpoint of the published size with the published root, and the database can only get there if something signs, which the refusal prevents. The README says the alert "clears itself when the database holds what was published"; after a real restore it never does. `Replace` is described as "the operator's explicit acceptance after a restore" but no operator call reaches it. The one way out is to remove `published.json` on every holder at the same time (a leader without the file asks the others first and gets the record back, as `TestTheRecordIsAskedForFromTheOtherHoldersWhenItIsLost` shows), and that way is exactly the second history the record exists to prevent: the restored database signs other aggregate roots for sizes that were published. Design 9.4 still says "the publisher re-derives tiles (same leaves give the same bytes) ... re-signed checkpoints are byte-identical", which the review of the collector showed false for the aggregate log and the events.

Why it matters: the documented disaster-recovery case ends in either a publication that has stopped for good (with anchors running on old checkpoints) or an unsafe manual edit that no document describes.

A fix must achieve: a documented, deliberate procedure (for example an explicit operator action that records the acceptance in a signed event and starts a new epoch of the aggregate log, or recovery of the missing rows from a mirror), a text of 9.4 that matches 171, and a test of the whole sequence.

### P7A-03 (medium): the credentials of a target are handed to the holders once, best effort and silently; a server that missed them cannot anchor, and nothing says why for hours

Place: `internal/ledgeranchor/rest.go putCreds, dropCreds`, `cmd/kingresd/ledger_anchor.go` (`each`), `service.go build`, `agent.go`.

`Distribute` runs once at create and at update; a failure is a warning in the log and the REST answer is still 201 or 200. There is no catch-up: a holder that was down, a member added later, or a host restored without the key directory never gets the file, and `CredStore.Raw` is read only to decode. After a failover to such a server `build` fails with "this server has no credentials file", which `work` treats as a retryable `target_unavailable`.

Steps (`TestRevNoCredentialsFileAfterFailover`: the file of the target removed, then jobs and monitor run every 20 minutes). Output: no alert at 1, 2, 3 and 4 hours (records pending, attempts 3, 6, 8, 9); `ledgerAnchorBehind` warning at 5 hours; the text of the record names the missing file, the alert does not. By the limits of the library the record fails, with `ledgerAnchorFailed`, only after 7 days or 200 attempts. The Test button was not run by anyone in that time. The same one-shot rule leaves the file of a deleted target on a holder that was down at the delete (`dropCreds` logs a warning).

Why it matters: a leader change, the case the design is built around, can end anchoring for a day with a warning four hours late and an error text that is in no alert.

A fix must achieve: a server that is about to lead (or a holder at start) obtains the files it lacks from the leader or the other holders; a failed distribution is returned to the caller as a warning and raised as an alert; an unbuildable target (missing or unreadable credentials, a bundle that cannot be read) fails the record at once with `ledgerAnchorFailed` and the cause in the alert text.

### P7A-04 (medium): a target that hangs holds the whole pass, not one place of it

Place: `internal/ledgeranchor/jobs.go` `pass`, `workTarget`; README "A target that hangs holds one place of a pass, not the pass".

The records of one target are worked on one after the other, each up to `SubmitTimeout` (90 s) or `UpgradeTimeout` (5 minutes), and `pass` waits for every target (`wg.Wait`). The scheduler runs a job again only `Every` after it returned.

Steps (`TestRevHangingTargetHoldsThePass`: five databases, one target whose store never answers a PUT, timeout of a call 400 ms). Output: `one pass over 5 records of one hung target took 2.0s`. With the defaults, 40 databases are 1 hour per submit pass and 3 hours per upgrade pass, during which no other target gets a new record submitted, a new checkpoint, or "Anchor now".

Why it matters: one dead calendar or bucket delays all the others and makes healthy targets raise `ledgerAnchorBehind`.

A fix must achieve: the targets are independent of the end of the pass (a per-target in-flight guard and no wait), or a bound on the time one target may take in one pass, and a test that a healthy target is submitted while another hangs for several of its records.

### P7A-05 (medium): the one place where a record becomes anchored does not check that the evidence belongs to the record

Place: `internal/ledgeranchor/jobs.go succeed`.

`succeed` accepts any receipt whose evidence parses canonically (`anchor.CanonicalEvidence`). WORM evidence names the key and `object_sha256`, OpenTimestamps evidence the digest, TSA evidence a token whose imprint a verifier compares; the wiring compares none of them with the payload of the row. The libraries do bind the evidence to the payload they were given (I found no case where they do not), so this is a missing second check, but it is the single gate to `anchored` and to the flag `ledger_checkpoint.anchored`.

Steps (`TestRevEvidenceOfAnotherCheckpointIsAccepted`): anchor size 3; a record of size 4 is begun; `succeed` is given the receipt "anchored" with the evidence of size 3. Output: `record of size 4 now anchored with evidence of size 3`.

Why it matters: a fault in one library, or in a kind added later, would be written as an anchor of the wrong checkpoint, and the stored evidence would not show it until a verifier reads it (steps 17 and 20 will serve it).

A fix must achieve: before `ApplyReceipt`, the wiring compares what each kind states with the row (WORM: key tail `<aggregate>/<size>` and `object_sha256` with the SHA-256 of the stored note; OTS: `digest`; TSA: the imprint in the token with `payload_sha256`), and fails the record as permanent when they differ, with a test per kind.

### P7A-06 (medium): one answer that names another chain ends the real chain's publication for good, with signed events, from a single round

Place: `internal/ledgerround/chain.go` (the `chain-replaced` branch after the check of P8-04), `closedLog`.

The fix of P8-04 raises a critical alert and refuses to retire when the answer names the open chain of another group. A chain id that is new (a forged answer, a mapping fault, a group briefly answering from another database) still retires the real log at once, with a signed `chain-retired` event, and creates a log for the new id. There is no confirmation over time like the 30 minutes of the vanished rule. When the real chain answers again it is "a closed chain that comes back": alert only, nothing published until the epoch of step 20.

Steps (`TestRevOneWrongAnswerEndsTheRealChain`): two groups; one round in which group 0 answers a chain with another id; then the real chain answers again with 4 more blocks for three rounds. Output: the real log `closed`, detail `retired: chain-replaced`, size 3 (the chain has 7 blocks); a log for the forged id created and closed again when the real answer returned; two signed `chain-retired` events, reason `chain-replaced`.

Why it matters: the precondition is a holder of the internal certificate or a transient fault, but the effect is irreversible and public, and the alert is the only signal.

A fix must achieve: the same shape as the vanished rule (the same new id in every round for a period, an alert from the first answer), or the retirement left to the operator's choice until step 20 exists, and a test with one wrong answer between right ones.

### P7A-07 (low): the result of a call is applied although the target was changed or switched off while the call was in flight

Place: `internal/ledgeranchor/jobs.go work, apply`, `pgstore.go Apply`; migration 0032 says the target `revision` exists "so that a job that built a client from an older configuration is told so when it writes its result"; nothing compares it.

Steps (`TestRevResultOfAnOldConfigurationIsApplied`: the hook `after-call` disables the target through `UpdateTarget`). Output: `target enabled=false revision=2; record state=anchored`.

Why it matters: a target switched off (compromised bucket) or re-pointed (another endpoint or credentials) is still credited with the record of a call made before the change, and the checkpoint can be flagged `anchored` through it. The evidence is true for the old configuration; the state of the target says otherwise.

A fix must achieve: `Apply` takes the revision the target had when the client was built and drops a result when the row is not at it, or when the target is disabled.

### P7A-08 (low): an old failed record keeps the critical alert after the newest checkpoint was anchored, and the only ways to clear it are a Retry of an obsolete record or deleting the target

Place: `internal/ledgeranchor/pgstore.go Failures`, `alerts.go Wanted`, `rest.go Retry`.

Steps (`TestRevOldFailureKeepsTheAlertAfterANewAnchor`: size 3 fails (`not_locked`), size 4 is anchored). Output: records `[failed anchored]`, `ledgerAnchorFailed target:1 critical` still open. A Retry of the record of size 3 submits an old checkpoint to the store again (another object under the key of the bucket).

A fix must achieve: a failed record whose (target, log) has a newer anchored record does not keep the alert (or the alert says it is stale and a "dismiss" exists), and a Retry of a record that a newer one has replaced is refused.

### P7A-09 (low): the evidence file is served for a record in any state and says nothing of the state

Place: `internal/ledgeranchor/rest.go Evidence`, `anchor.EncodeEvidenceFile` (no state member; `created_at` is the row's `updated_at` when the record is not anchored).

Steps (`TestRevEvidenceFileOfANonAnchoredRecord`): a record in state `submitted` returns a canonical `anchor-N.json`. For OpenTimestamps the evidence itself says `bitcoin.status: claimed` or null, which a careful reader sees; the file and the name do not. `kingres_readonly` may download it.

A fix must achieve: the file carries the state of the record, or `json` is refused for any state but anchored and the states that the design says hold something to keep (6.2), and `created_at` is not the time of the last write.

### P7A-10 (low): the redactor knows the raw value only; short secrets are not redacted at all

Place: `internal/anchor/secret.go Redactor` (values shorter than 4 bytes ignored), `ledgeranchor/creds.go credMembers` (TSA `username` and `password` may have 1 character), `tsa/target.go reason` (the free text of a TSA answer, 100 characters, goes through the redactor into the record and the alert).

Steps (`TestRevRedactorForms`): secrets `s3cr3t-pw`, `bob`, `abc`; text with base64 of `bob:s3cr3t-pw`, `abc`, and the URL-escaped form. Output: all of them unchanged. A server that echoes the `Authorization: Basic` header in the status text of a rejection puts the base64 of user and password into `last_error`, the alert and the list, which `kingres_readonly` reads. Not found in the libraries' own errors, which are static; the reach is the free text of the TSA and the stored message of a store.

A fix must achieve: the redactor also removes the base64 of `user:password`, the URL-escaped and hexadecimal forms, and the credentials validation refuses members shorter than the redactor's minimum (or the redactor takes every length).

### P7A-11 (low): when the record of what was published cannot be written, the publisher goes on and nothing is raised

Place: `internal/ledgerround/extend.go` (the error of `RecordCheckpoint` is logged), `ledgersign/service.go mutate` (the error of `RecordEvent` is logged).

Steps (`TestRevRecordWriteFailureIsSilent`: the key directory is removed after the first round, then a round with 2 more blocks). Output: `SignErr=<nil> published=true`, alerts only clears. The in-memory record is current until the process ends and the other holders get it from the next sync; a single-server cluster loses it at a restart. The README says a crash between the store and the record is harmless; a disk that is full is not a crash.

A fix must achieve: a failed write of the record is a critical alert and stops signing until it is written (the record is the guard of P8-01).

### P7A-12 (low): the cause of a failing `credentialsPath` is printed as `%!w(<nil>)`

Place: `internal/ledgeranchor/service.go build`: `fmt.Errorf("...: %w", errors.Unwrap(err))`, where `ReadCredentialsFile` returns the bare sentinel `ErrCredentials` or `os.ErrNotExist`, which unwrap to nil.

Steps (`TestRevCredentialsPathMissingText`): create a target with `credentialsPath`, delete the file, press Test; then write `not json`. Output for both: `...with the members of the kind): %!w(<nil>)`. The operator cannot tell a missing file from a bad mode or content.

A fix must achieve: the error says which of the three it is (without a path or content).

### P7A-13 (low, read only): two enqueues of the same checkpoint race on the unique key and one of them fails

Place: `internal/ledgeranchor/pgstore.go Enqueue` (select, then insert in one read committed transaction, no lock, no `ON CONFLICT`), `jobEnqueue` and `AnchorNow` run on the same leader.

A concurrent "Anchor now" and the enqueue job can both pass the "covered" check and insert; the second gets the unique violation (`23505`), which `Enqueue` does not map (only targets do): "Anchor now" answers 503 and the job logs a warning. No double record results. Not run (PostgreSQL store).

### P7A-14 (low): limits of the record file

Place: `internal/ledgerstate/state.go` (`MaxFile` 4 MiB checked by `Open` and by the merge call, not by `save`; no sync of the directory after the rename).

A log entry is about 230 bytes, so about 18 000 logs fill the file; a file that is larger after a save is read as damaged at the next start and refuses all signing (P7A-02 has no way out). The merge body is limited to the same size. After a power loss the rename may be lost; the record is then one write behind the database, which is the harmless direction.

### P7A-15 (note): decisions that should be written down

- `credentialsPath` and `caBundlePath` take any path the daemon can read; a failed read tells an administrator whether a file exists and whether it parses. A JSON file with the right member names would be sent to the endpoint of the target (Basic password in clear for the TSA). Administrator only.
- An address written as an IP in the configuration is dialled as written, including link-local and loopback (documented). Combined with `Test` this lets an administrator send requests to any host:port of the network from the daemon.
- `kingres_readonly` reads the configuration of every target (including the path of `credentialsPath`), the last test and `lastError` of every record. The Test button leaves a probe object per press; in a bucket with a default compliance retention it cannot be removed.
- `anchor_minimum` is compared with the targets only when it is set; the flag `anchored` is not computed again when it changes.

### P7A-16 (note): the 30-minute clock of a vanished database is in memory

`missingSince` is a map in the collector: a restart or a leader change starts the 30 minutes again. This is the safe direction, but a cluster whose leader changes more often than every 30 minutes never ends the publication of a database that is gone.

### P7A-17 (note): gaps in the tests of the authors found by the mutants of section 6

Mutants that survive, in the order of what they guard: a damaged record replaced by what the database holds (A15, now covered by my `TestRevDamagedRecordIsNotReplacedByTheDatabase`); the root of a published size compared in `Check` (A11b, my `TestRevSameSizeAnotherRootIsRefused` kills it); an unreachable group counted as a vote that a database is gone (A14b, my `TestRevAnUnreachableGroupIsNotAVote`); the evidence that is given only for the database named in the path (B15); a sealed credentials file with fewer iterations than the minimum (B19); the redaction of the error of a build, of the text of a failure and of the result of a test (B06, B07, B20: layers over a library that already cleans, none has a test that needs it); the 3 times interval of the behind warning (B08); the start of the clock at the switching on of a target (B11); the claim of a record in a process (B28); the egress rule of the virtual-host name of a bucket (B27b); the leadership that ends during a call (B02); a receipt whose evidence does not read back (B01). The mode check of a credentials file (B18) cannot be killed on Windows and was not run on Linux.

## 4. What held

Part (a):
- P8-01. With the record present, a restored database that is behind (sizes 1 of 3), that holds the same size with another root, that holds fewer events or another last event, signs nothing, writes nothing and raises `ledgerSignerUnavailable/published-state` (the tests of the authors and `TestRevSameSizeAnotherRootIsRefused`); a lost record is asked for from the other holders; a damaged one refuses; the checks run at the start of every round, at every retirement and before every event. All checkpoint signing goes through `ensureCheckpoint` (`Allow` and `RecordCheckpoint`), and every event through `mutate`. The monotone merge refuses a fork.
- P8-02: the retirement ends while the other key keeps signing; the refusal looks at notes signed by the retiring key with the key id of each origin (A08 killed).
- P8-03: a healthy database whose groups said "no database" for less than 30 minutes, or that the system instance still has, is not closed (A09, A10b, A17 killed); a closed chain that returns is an alert and nothing is written.
- P8-04: two swapped answers diverge both logs and retire nothing (read and the test of the authors); P8-05 to P8-07 read only.

Part (b):
- A lying store (no lock, governance for compliance, short retention, bad checksum header, bad length, bad body, no version id, another version at HEAD, no last-modified, store time two hours early or late) never anchors through the whole wiring (`TestRevLyingStoresAnchorNothing`; a store whose bucket reports versioning suspended still anchors because the library judges the object, not the bucket flags, which are checks of the Test button only; I take that as the design).
- A record is anchored only from the receipt of a call; the state changes through `anchor.Record`; a claimed record is shown as submitted and never counts; an expired record needs a Retry.
- A kill at any point of a submit, a deposed leader and a follower write nothing (term guard in every store write, compare and swap on the row version); an in-flight result after a Retry, a supersede or a delete is dropped. I found no case of a double record or a lost anchor from a leader change.
- Credentials: the audit entry says `credentialsChanged` only; no answer, row, log line or alert of the authors' scan or mine held a raw credential; the sealed file binds its header; the files are written atomically with mode 0600; `{}` removes them; the agent takes a number for the path.
- Egress: the dialer refuses any host:port the configuration does not name (B16, B17 killed).
- The alerts of 6.9 follow the state: the "not anchored" alert clears when a target is enabled, the behind alert is clocked from the switching on of the target, a Retry or a delete clears the failure alert, a restart changes nothing (read and the tests of the authors).

## 5. Alerts against the design

Raised as 6.6 and 6.9 say: `ledgerNotAnchored` (info, warning with `anchor.required`), `ledgerAnchorBehind` (warning, critical), `ledgerAnchorFailed`, `ledgerAnchorConflict`. Not raised when they should be: a target that cannot be built (P7A-03, hours late), a hung target (only the behind alert, P7A-04), a failure that a later success makes stale (stays, P7A-08), the end of a WORM retention (a log line once an hour, as the design allows).

## 6. Mutants

Each in a clean copy of the tree, a build and the unit packages `ledgerstate`, `ledgerround`, `ledgersign` (part a) or `ledgeranchor` (part b).

Part (a):

| id | change | result |
|---|---|---|
| A01 | `Allow` in `ensureCheckpoint` removed | survived (the check at the start of the round covers it; defence in depth) |
| A02 | `Allow`: smaller size accepted | killed |
| A03 | `Allow`: same size, other root accepted | killed |
| A04 | event count check loosened | killed |
| A05 | no `checkPublished` in the round | killed |
| A06, A10, A11, A14 | first form did not compile | not counted; redone as A06b, A10b, A11b, A14b |
| A06b | no `AllowEvent` in the key service | survived (`PublishedCheck` runs first) |
| A07 | no check at a retirement | killed |
| A08 | the retirement bound by the largest size of the cluster again | killed |
| A09 | vanished: time ignored | killed |
| A10b | vanished: catalog ignored | killed |
| A11b | `Check`: root of a published size not compared | survived the authors' tests; killed by my test |
| A12 | `RecordCheckpoint` does nothing | killed |
| A13 | `Merge`: fork accepted | killed |
| A14b | an unreachable group counts as "no database" | survived the authors' tests; killed by my test |
| A15 | damaged record not refused | survived the authors' tests; killed by my test |
| A16 | `AllowEvent`: last event not compared | killed |
| A17 | `noteMissing`: the clock is not reset by another answer | killed |

Part (b):

| id | change | result |
|---|---|---|
| B01 | evidence that does not read back is not a failure | survived |
| B02 | no stop when leadership ends during a call | survived |
| B04 | no expiry | killed |
| B05 | the evidence of a partial failure not kept | killed |
| B06 | no redaction of the text of a failure | survived |
| B07 | the error of a build not redacted | survived |
| B08 | behind warning at 3 intervals, not 3 plus an hour | survived |
| B09 | behind critical at 12 intervals, not 12 plus an hour | killed |
| B10 | `anchor.required` does not raise the severity | killed |
| B11 | the lag not clocked from the switching on | survived |
| B12 | alerts are not cleared | killed |
| B13 | a change of severity not written | killed |
| B14 | Retry of a record of another database allowed | killed |
| B15 | evidence of a record of another database allowed | survived |
| B16 | egress allows every host | killed |
| B17 | egress allows loopback names | killed |
| B18 | mode of the credentials file not checked | survived (the check does not exist on Windows) |
| B19 | sealed file with too few iterations accepted | survived |
| B20 | result of a test not redacted | survived |
| B21 | evidence of a record with none not refused | survived |
| B22 | a claimed record with no next check not worked on | killed |
| B24 | kind of a target may change | killed |
| B25 | enabled WORM target with no credentials accepted | killed |
| B27, B27b | first form did not compile; B27b egress for a virtual-host bucket | B27b survived |
| B28 | a record may be worked on twice in one process | survived |
| B29 | an older pending record not superseded (memory store) | killed |

The PostgreSQL store has no mutant: its tests did not run here.

## 7. What was not covered

- The PostgreSQL stores of anchoring and publication (`Due`, `Enqueue`, `Apply`, `Failures`, `signedByMax`): read, not run. P7A-01 and P7A-13 are shown on the memory store or from the SQL.
- No cluster test ran. The two named above stayed in the queue of the machine-wide mutex for the whole session; a cluster test of a restore of `kingres_cp` from a backup (which would show P7A-02 on a real cluster) does not exist and was not written.
- The libraries (`worm`, `sigv4`, `tsa`, `ots`) were not re-read; the fake time-stamp authority and the fake calendars are those of the authors, and I wrote no new one. The OpenTimestamps kind with a header source was not driven by a fake explorer of my own.
- P8-05, P8-06 and P8-07 (rate table, `ReplaceNote`, adoption marker) and the `logs.json` file were read, not tested.
- The console, the REST contract text and the verifier were not looked at; the mode checks of files were not run on Linux.
