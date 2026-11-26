<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->

# Independent review of the signing keys of the ledger in kingresd (phase 7, steps 3 and 4)

## 1. Scope and method

Reviewed: `kingres/control-plane` (`internal/scheduler`, `internal/ledgersign`, `internal/sysdb` runner and migration 0030, the seven REST handlers in `internal/httpapi/handlers_ledger_signing.go`, the three internal calls in `internal/internalapi`, the wiring in `cmd/kingresd/ledger_signing.go`), contract 0.11.0 in `kingres/api`, design `ledger-phase7.md` (section 10 rows 3 and 4, sections 2, 3.2 to 3.5, 6.7, 8.2 to 8.4, 9.x, corrections 93, 94, 101 to 103, 107, 121 to 136), the key and log library `doc.go` duty lists, and `kingres/verifier/README.md`. The libraries `ledgerkeys` and `ledgerlog` were not reviewed, only their use.

Method: reading of every file of the packages above; probes written as extra tests in a private copy (in-memory store, fake clock, the author's own harness); one-line mutants of the author's code against the author's tests. All line references are to the committed export.

### What was run, and what was not

| Run | Result |
|---|---|
| `go build ./...`, `go vet` of scheduler, ledgersign, sysdb, httpapi, cmd | clean (Windows) |
| `go test` scheduler, ledgersign, sysdb, httpapi (Windows, in-memory store; tests that need PostgreSQL skip there) | all ok |
| Five probes of my own (in-memory store, Windows) | results in the findings below |
| Twelve one-line mutants (Windows, ledgersign package) | table in section 6 |
| PostgreSQL-store tests (`TestADeposedWriterCannotWriteOnTheDatabaseStore`, kill tests on the database store, runner tests with gaps), the cluster test `TestLedgerSigningKeysAreMadeDistributedRotatedAndRevokedAcrossTwoServers` | **NOT RUN, on either major.** My run queued behind the integration mutex, which another full run then held for about two hours; I did not wait. Everything about PostgreSQL behaviour (the `FOR SHARE` guard, deadlock mapping, the runner under a real database, two servers) was only read. Counts per major: 0 on PostgreSQL 18, 0 on PostgreSQL 19. |

## 2. Duties of the caller

Duties of `ledgerkeys` (and the one of `ledgerlog` that concerns keys) against the daemon.

| Duty | Verdict | Where / note |
|---|---|---|
| Persist the set after every transition | fulfilled | `PGStore.Commit`, one transaction with revision CAS |
| Serialise transitions | fulfilled | task subject `ledgerKey/signing`; `keys_revision` CAS; `ErrConflict` retried 5 times (`service.go` `mutate`). One small TOCTOU, P7-15 |
| Clock from a trusted source, compared with a second one | **dropped in effect** | `service.go` `clock`: the "second source" is the database on the same host, P7-02 |
| Pass `min(now, RecordLimit())` after idle | fulfilled | `recordTime`; tested |
| AddChecked with every origin | fulfilled | `stepGenerate` uses `origins` (cluster prefix and every `ledger_log.origin`) |
| CheckSetKeyIDs when a new origin appears; TrustFile with the same origins | **handed on, not stated** | no code path; the origins come with step 5 (logs). Not written in the README hand-over list. `load` rebuilds the trust file without origins (`service.go` `load`), so a collision with a later log origin is invisible to the comparison |
| Validate after every load | fulfilled | `load`; alarm, no key task starts, no signing |
| Keep the published trust file and compare after every load | fulfilled, with the limit of P7-05 | `load`, `case !bytes.Equal(...)`; the stored copy lives in the same database as the rows |
| Choose the TreeBound at Retire | fulfilled at the event step, **race open** | `stepEventRetired`, P7-01 |
| Re-sign the latest checkpoint at each rotation | handed on to step 5 explicitly | `stepResign` records a skip; `Config.Resign` nil |
| During an overlap sign with every signer | handed on to step 5 | `Service.Signers` returns all |
| Build, sign and log key-added, key-retired, key-revoked with CanSign | fulfilled | `signEvent` calls `CanSign` for every signer |
| Poll DueForRetirement and retire | fulfilled | job `ledger.key.due`, every minute |
| Publish trust file; delete the file of a retired or revoked key on every holder; back up | partly | trust file served; deletion fulfilled for reachable holders, **not for a removed holder** (P7-03), temp files (P7-09); backup only a reminder flag |
| Alarm on Validate failure, ErrNoActiveKey | fulfilled | `ledgerSignerUnavailable` |
| Alarm on a due retirement, 14 or more keys, every revocation, ErrTooManyKeys | **dropped** | only one alert kind exists; no code raises anything else (P7-11) |
| Alarm on an unopenable key file | **dropped** | P7-04 |
| Never, automatically: revoke, Unbounded, Add, activate a stale next key, take an outside time | fulfilled | none of them occurs; the stale-next rule is the 300 days in `stepGenerate` |
| (ledgerlog) Checkpoint write under a term guard, signing and retirement exclusion | handed on to step 5 explicitly (README, correction 129); the hand-over is incomplete, P7-01 |

## 3. Findings

No finding is critical. No private key, seed or passphrase reached a log line, an error text, an API answer, a task row, an audit entry, an event or an alert in anything I ran or read, except as noted in P7-07 (memory only) and P7-08 (a path, not a secret).

### P7-01 (medium): the bound of a retired key is fixed in the first step and never checked again; `Signers` keeps offering the key meanwhile

Place: `internal/ledgersign/tasks.go` `stepEventRetired` (reads `MaxCheckpointSize` outside the transaction that commits the event), `stepRetire` (takes the bound from the stored event text only), `Service.Signers` (`tasks.go`, offers every active key), README "What steps 5 and 6 will find".

Steps (probe, in-memory store): create key, rotate, advance the clock past the overlap, set the largest stored checkpoint to 100, start `ledger.key.retire`, and in the engine fault point after step 0 call `Signers` and raise the largest stored checkpoint to 5000 (a checkpoint the collector signed with the retiring key).

Output:

    between the event and the retirement Signers returns 2 signers (err <nil>)
    retired with bound 100 while a checkpoint of size 5000 is stored

The key is retired with `max_tree_size` 100 and a checkpoint of size 5000 signed by it is refused by the verifier for every auditor. Correction 129 hands the exclusion to step 5, but today (a) nothing in the stored state or in the `Signers` answer tells the collector that a retirement is under way (the event exists, the key is still `active`), (b) the window is not seconds: a retire task that fails at a retryable step (clock, `term_over`) keeps the event-without-retire state for as long as it takes to retry, (c) `stepRetire` does not compare the bound with the largest stored checkpoint at the time it commits, so even a collector that behaves cannot be detected as having raced. As it stands the hand-over is not safe: a key can be retired today with a bound that a later checkpoint signed by the same key would exceed, and nothing refuses it.

A fix must achieve: a signing path that cannot use a key once its key-retired event exists (for example `Signers` skips such a key, using the event table in the same load), and the retire commit re-reads `MaxCheckpointSize` inside the guarded transaction and refuses (task error, not a retire) when it exceeds the bound stored in the event; a test with a checkpoint stored between the two steps.

### P7-02 (medium): the clock "second source" is the same clock

Place: `internal/ledgersign/service.go` `clock`; `cmd/kingresd/ledger_signing.go` `sys`; `internal/cluster/cluster.go` `ServiceConn` (lines 384 to 392).

Reasoning (read): `Store.DBTime` runs `SELECT clock_timestamp()` on the connection `ServiceConn` returns, which goes to the primary of the map group on this server through its unix socket. The leader is the server that runs that primary, so the database clock is the clock of the same host, read from the same operating system clock. The test `TestATimeComesFromAClockThatAgreesWithTheDatabase` uses two fake clocks and proves only the comparison, not that the sources differ. A clock that is wrong by an hour passes the check; the library's duty ("compare with another host, the database, a signed time") is met in letter and not in effect. Consequences the library doc lists: a first key activated in the future is not in force until the real time arrives (no signing, no way back but waiting); `jobDue` retires a key early when the clock jumps forward past the overlap, irreversibly. The internal call `GET /time` (any peer) and the `clockDifference` alert exist and would give a real second source.

A fix must achieve: the comparison uses a clock of another host (the other holders through `GET /time`, with a majority or a bound), or the transitions refuse while the `clockDifference` alert is open; at least the README must say that the database check is vacuous on one host.

### P7-03 (medium): a holder that was removed from the cluster and cannot be reached is never forgotten; it makes a task every minute; a revocation can never finish; its key file stays

Place: `tasks.go` `removeStale` (non-strict branch records `union(good, pending)`), `jobSync`, `install`; `membership/remove.go` has no call into the key directory.

Steps (probe, in-memory): two peers hold the key; one is removed from group 0 and made unreachable for good; run the sync job five times; then revoke the key.

Output:

    pass 0..4: started 1 tasks each, holders [s1 s2 s3]
    revoke task: failed {Code:key_files_remain ... could not be removed from: s3 ... Retryable:true} ; key state revoked
    second revoke: conflict: The key is revoked already.
    file of the revoked key still on s3: true

`dirty` is true on every pass because `holderSet` (without the removed server) never equals `Cluster.Holders` (which keeps it for ever), so `ledger.key.distribute` is submitted every minute for ever (about 1,440 task rows a day, no retention exists: `grep` for pruning in `internal/task` and `internal/ops` finds none; design 6.7 rejects exactly this "task per minute"). The README says an unreachable server "never makes a task": true for a server that is a holder and down, false for one that is no longer a holder. A revocation ends `failed/key_files_remain` after its irreversible step, and the operator cannot start it again ("revoked already"); only the lenient distribution could clean up and it cannot reach the server. Nothing removes the key files when a server is removed from the cluster, so a decommissioned host keeps a copy of the active key. The same loop (one `done` distribute task a minute) occurs for a reachable holder whose passphrase differs (`install` fails, `jobSync` sees the file missing every pass); that part I only read.

A fix must achieve: a server that is no longer a member leaves `Cluster.Holders` (with a recorded, visible "key files may remain there" state, an alert, and an explicit operator action to confirm it was wiped); removal of a server cleans its key directory while it is reachable; the sync job does not start a task whose outcome cannot change; a revocation whose last step fails can be completed without a second revocation.

### P7-04 (medium): a key file that exists but cannot be opened is not an alarm, the alert is cleared, nothing repairs it

Place: `service.go` `alarmText` (only `Keyring.Has`), `tasks.go` `jobCheck`, `jobSync`, `Keyring.List`.

Steps (probe): create a key with a second holder, overwrite the key file on both servers with a garbage line, forget the cached signer, then call `Alarm`, `Signers`, the check job and the sync job.

Output:

    Alarm on the leader: "" <nil>
    Signers: ... the key 28811f4f60e867fc cannot be opened: ... the first line is not KINGRES-KEY-1
    alerts: [ledgerSignerUnavailable/clear/]
    sync job started 0 tasks

`getLedgerSigning.signerAlarm` is null and the critical alert is cleared while signing is impossible. The same holds for a file sealed with another passphrase, a truncated file, a file of another key under a right name, and for every other holder: `List` returns names, `install` skips a name that is present, so a corrupt copy on a leader candidate is found only when that server becomes the leader and cannot sign. This defeats the purpose of distributing to candidates. (The contract text says the alert covers "the key file is missing on the leader", so this is a gap in the property, not a contract violation; correction 134 says the alert covers a missing file.)

A fix must achieve: the check job opens the files of active and next keys on the leader (and the holders verify theirs through the list call, for example by returning the public key each file opens to), compares them with the rows, and raises the alert and the `signerAlarm` on any failure; a holder whose file does not open is given the file again or reported.

### P7-05 (medium): the key rows, the stored trust file and the events are one trust domain; stored events are used unverified

Place: `service.go` `load`; `tasks.go` `activationTime`, `stepRetire` (parse `not-before` and `max-tree-size` from the stored note text without checking its signature), `appendEvent` (`seq := len(Events)`).

Reasoning (read, not run): the comparison "trust file built from the set equals the trust file stored" protects against a partial change. An attacker with SQL write access to the system database and no key file can write rows and the stored trust file together: add a row for their own key as `active`, or flip a revoked row back to `active`, and write the matching `trust_file`. `load` finds no difference; `getLedgerTrustFile` serves the file from the same table. The daemon never verifies that the events are contiguous, that each is signed by keys of the set, or that the state of every row follows from them. The attacker cannot make the daemon sign with their key (no file: `Signers` fails), and an auditor that follows the contract (the fingerprint obtained out of band; events signed by a pinned key) is not led to trust the key. What they can do: make the daemon publish a trust file that lists their key or un-revokes one (misleading anyone who takes the file from the API, which the contract forbids but does not prevent); rewrite the unsigned-checked text of a stored key-retired event so the next retire step uses `max-tree-size: 1` (every checkpoint of the retired key then refused by every auditor) or a huge bound; delete one event so the next event takes a number that already exists and every key task fails with a conflict for ever (`mapConflict` maps the primary key violation to `ErrConflict`, five tries, then the task fails).

A fix must achieve: `load` verifies the event chain (numbers without gaps, each signed by a key that the earlier events authorise, the first by the stored first key) and that every row's state and times are what the chain implies; the activation time and the bound are read from verified notes only; the daemon keeps a hash or a signature of the trust file that an SQL writer cannot forge (in the key directory, sealed, or signed by the active key and checked against files).

### P7-06 (low): the revoke event and the key record can carry different `compromised-since`

Place: `tasks.go` `stepEventRevoked` and `stepRevoke` each call `clampSince(in.CompromisedSince, t, k)` with their own `t`.

Probe: request `compromisedSince` 60 s in the future (the API allows up to the 2 minute skew), let 10 s pass between the two steps.

    event since=2026-11-18T19:20:38Z record since=2026-11-18T19:20:48Z

The signed event says an earlier time than the record and `getLedgerSigning`; the record is the later (less safe) one. Correction 125 says "the event and the record carry the clamped value". The API check also says "must not be later than now" while it allows two minutes.

A fix must achieve: the clamped value is decided once, written in the event, and the revoke step reads it back from the event as the retire step reads its bound.

### P7-07 (low): the seed text is held in unzeroed strings on the install path, and the memory scan does not cover that path

Place: `tasks.go` `installOn` (`b64(data)` string; `req.File = ""` does not erase it; the JSON body in `peerclient`), `agent.go` `Handle` (body read with `io.ReadAll`, `in.File` string), `secretscan_linux_test.go` (the doc comment says the path "is not part of this flow, which has no peer, and is named in the documentation").

For a plain key file the base64 of the seed is in the heap of the leader and of every holder for an undefined time; core dumps are off only through the unit file the packaging will ship (`LimitCORE=0`; no `RLIMIT_CORE` or `PR_SET_DUMPABLE` in the daemon: grep). A deliberate, documented limit, not measured: the scan test exercises the no-peer flow only and searches the base64 text only. Not searched by the author's secret search (`env_test.go` `secrets`): upper-case hex, base64url, a JSON-escaped form, the decimal list a `%v` of a byte slice prints, the 64 byte expanded private key as a unit, and the passphrase in other encodings.

A fix must achieve: the install body is built and read as bytes that are cleared; the memory scan runs the install path with two servers; `RLIMIT_CORE=0` and non-dumpable set by the daemon itself.

### P7-08 (low): an absolute path reaches a task error visible through the API

Place: `service.go` `signEvent` (`coded{code: "signer_unavailable", msg: fmt.Sprintf("... %v", err)}`), `tasks.go` `Signers`.

The probe above printed the whole key file path (`...\s1\signing\<fingerprint>.key`) inside the error that comes from `Keyring.Signer` through `ledgerkeys.ReadFile`. A task error is stored and shown by `getTask` to a readonly role. The path is not a secret; it discloses the data directory layout. `Keyring.File` and `pathError` reduce errors, `Signer` does not.

A fix must achieve: errors from key file reads name the fingerprint only.

### P7-09 (low): a temporary key file survives a crash and is never cleaned

Place: `ledgerkeys/keyfile.go` `writeAtomic` (`.key-*` temporary file, hard link, then remove) as used by `Keyring.Generate`/`Install`; `Keyring.List` ignores names that are not `<fingerprint>.key`; `Keyring.Remove` removes only that name.

A crash (or a failed remove) between creating the temporary file and the link leaves `.key-XXXX` with the full key file (plain seed in clear text mode) in `signing/`. It is not listed, not adopted, not removed when the key is retired or revoked.

A fix must achieve: `Keyring` removes `.key-*` files older than a minute at start and in the distribution step, and the test kills between the write and the link.

### P7-10 (low): adoption of a leftover key file rests on a modification time anyone with write access can set

Place: `keyring.go` `Orphan` (`!st.ModTime().Before(since.Add(-slack))`), `stepGenerate`.

A file with no row is adopted if its modification time is not earlier than the task creation less two minutes. Anyone who can write to the key directory can place a file and set its time; the next create or rotate task adopts the key and the active key signs its `key-added` event, so the attacker's key becomes one that auditors accept. On a server with a passphrase file the planted file must be sealed with that passphrase (`Keyring.Signer` refuses a plain file there), which an attacker who cannot read the passphrase cannot do; on a server without one, anyone who can write can read the active key too. So the extra capability is small. Correction 130 sets the rule; it is not a security boundary.

A fix must achieve: adoption needs a marker that only the daemon can write (the task id sealed in or next to the file) and is limited to the task that wrote it.

### P7-11 (low): stale next keys can fill the set; alarms of the duty list are not raised

Place: `tasks.go` `stepGenerate` (adopts only a next key with no event), `CreateKey` (refuses at 16), no alert for 14 or more keys, a due retirement that fails, or a revocation.

A create or rotate task cancelled after its key-added event leaves a next key that is never adopted again and counts toward the 16; every repeat adds one. Revoking such a key still keeps its place (correct per correction 135). A set full of stale keys can no longer rotate: the only way out is a new origin prefix. `kingres_admin` only, and cancelling is deliberate, but there is no warning at 14 (the library asks for one) and no alert for a revocation (only the audit entry). `jobDue` starts a new retire task every minute while a retirement fails permanently (for example `cannot_sign`): another loop of task rows.

A fix must achieve: reuse of a next key that has an event and no activation by the same kind of task; an alert or `signerAlarm` note at 14 keys; the due job backs off after a failed task.

### P7-12 (low): key files are written and removed outside the term guard

Place: `stepGenerate` (`write()` before `Commit`), `install`, `removeStale` (calls through `callServer`).

Every write of key state in the database goes through `Term.Guard` in `PGStore.Commit` (the settings operation, the tasks, the holders record and the events all use it; I found no other write to the three tables, and the alert and audit writes touch other tables). File operations are protected only by the callee's leader check and the fence header (`internalapi.go`, fence checked only when the header is present, and a lower epoch is refused only after the newer run has called the same callee). A leader that lost leadership between loading the state and its remove loop acts on a stale `wanted` and can ask a holder to delete the file of a key created since. The window is the resolver lag (about 2 s plus the poll) and exists only after a failover that is not built yet; the next leader's distribution restores the file only if the server still has a copy elsewhere (the leader's own copy is the source).

A fix must achieve: `removeStale` re-reads the set under the guard immediately before each remove and never removes a file of a key in state next or active.

### P7-13 (low): a leader change between "generate" and "install" ends the task without a way forward

Place: `tasks.go` `install` (`key_file_missing`, not retryable) after `stepGenerate` wrote the file only on the old leader.

If the task is resumed by another server before the file was copied, the new leader has the row (state next) and no file; the task fails for good, the row stays as a stale next key with a permanent `ledgerSignerUnavailable` raised by `alarmText` (a next key without a file) until revoked, and the first key of a cluster cannot be revoked (no other active key). Needs failover of the map group; read only.

A fix must achieve: the key is made on the leader, copied to the holders and recorded in one step, or the missing-file case makes a fresh key and drops the row.

### P7-14 (low): contract and documentation differences

- `LedgerSettingsCluster.publication.originPrefixLocked` and the 409 text say the prefix is locked once a checkpoint exists; the server (`UpdateSettings`, `settingsOf`) locks it as soon as one event or checkpoint exists (correction 121). The contract text is not updated.
- An empty `reason` is accepted (the schema has no minimum) and written as the line `reason: ` with a trailing space (probe output shows it in the signed note); harmless today, a trap for a strict event parser.
- A malformed fingerprint in the path gives 404 `not_found` in the service; the contract path pattern would give 400 if the generic layer did not catch it first (not run).
- `publicUrl` is any printable ASCII of at most 1024 bytes; no scheme check, while the console builds links from it.
- The default origin prefix is built from the server id of the current leader (`newLedgerParts(..., serverHost, ...)`), so the default shown for confirmation can change after a leader change and before the first key; the README and the design say "server address".
- The README numbers the corrections differently from the design (it says 133 is Content-Disposition, 135 the alert, 136 the 300 days; the design has 136, 134, 130/135).
- The README says the sync job "never makes a task" for an unreachable server (see P7-03).

### P7-15 (low, note): two small races and one idempotency note on the create operation

- `CreateKey` decides the kind (create or rotate) before `StartTask`; a request that read the set just before another creation finished can start a task of kind `ledger.key.create` that behaves as a rotation and has no re-sign step. Window: the length of one task.
- A client that repeats `createLedgerSigningKey` after a lost answer, once the first task has finished, starts a rotation; the contract says so ("Any later call is a planned rotation") and there is no idempotency key in the API. Note only.
- A step failure that the library causes from the clock (`cannot_activate`, `cannot_retire`) is not retryable; a clock that stepped back between two steps of one task fails it for good and leaves a next key with an event (P7-11).

### P7-16 (note): the migration runner

Read: `Run` takes the advisory lock, creates the record table, reads the applied versions, refuses (`ErrSchemaTooNew`) before applying anything if the database has a version the binary does not know, then applies each unrecorded migration in order, each in its own transaction together with its record. `CheckVersion` has the same refusal. Consequences, by reading:

- A binary that lacked a lower number which a later binary has: the later binary applies it after the higher ones. Safe today (no migration between 15 and 29 exists; 0030 only creates tables), a hazard for the day one of them alters something 0030 relies on. Nothing in the runner or in a test states or checks that a late migration is independent of the higher ones.
- Two servers of different versions starting at once: serialised by the advisory lock; the older one then finds the higher version unknown and refuses. The old strict refusal is kept for that direction.
- A migration that fails: its own transaction rolls back, the earlier ones stay, the next start continues. 0030 is one transaction.
- `MigrateSystem` runs at every start on every server's own system database (each server has its own copy; only the map primary's holds the cluster state), so the ledger tables also exist, empty, on followers. The store is reached only through `ServiceConn`, which uses the map primary where this server runs it, so a follower never writes real state; it was not run.
- The returned "version" is the highest applied, which no longer says that every lower one is applied; I found no caller that uses it as such.

The sysdb runner tests (`TestAMigrationInAGapIsAppliedLater`, `TestBinaryOlderThanSchemaRefuses`) need PostgreSQL and were not run here (they skip on Windows and were green only in the sense that the package built).

## 4. What held under attack

- The term guard is the first statement of the only code that writes the key tables (`PGStore.Commit`), and a deposed writer fails with `ErrDeposed` (read; the memory store reproduces the rule and the mutant that removes it is killed). Revision CAS makes two changes from one stored set unable to both commit. Settings, tasks, holders record and events all go through it.
- A job panic is recovered per pass, logged with a stack (no secret is on those stacks by reading), and the other jobs go on. A follower holds no term and answers 503 with `Retry-After`.
- The key operations are `kingres_admin` and the three reads `kingres_readonly` in the generated operation table; the follower forwards all seven to the leader; `Content-Disposition` is one of a fixed list of relayed headers (`relayedHeaders`), so a follower cannot be made to relay other headers by a leader answer.
- The audit entry of a revocation holds the task id and the stated time only; the reason is not copied (test and handler agree). The reason does appear in the task input row and in the signed, published event, which the contract says.
- Internal calls: `LeaderOnly` (caller certificate checked against the callee's own leader view), mutual TLS, fence header checked when present, body limit 1 MiB, strict base64, fingerprint regular expression (no path traversal), `key_invalid` answers carry library error texts that name no secret. No request body is logged on either side; the leader logs the server and the error.
- A plain key file on a holder that seals is re-written under its own sealing; a sealed file with another passphrase is refused with 409 `passphrase_mismatch`; a plain file on a sealed server is not adopted.
- Retire and revoke events are signed by another active key in force; the revoke of the last key is refused in the API and again in the event step; retire then revoke of the same key produces a consistent trust file (probe: `revoked: true`, bound cleared).
- `RecordLimit` use (mutant killed), the idle set, the 300 day rule, the key id check under every origin, the 16 key limit in `CreateKey`.

## 5. Questions not fully answered

- Q1 file permissions: `NewKeyring` creates `signing` with 0700 and files are written 0600; an existing wider directory is not tightened (note). Command line and environment hold only the passphrase file path. No metrics or debug endpoint in these packages returns key material (read).
- Q2 kill tests: the engine crash points are covered (`crash_test.go`), plus a kill inside one step; I did not find a test that kills between `write()` and `Commit` on a second server, between two `install` calls, or in the middle of `removeStale`; the two-leaders-for-a-short-time case is by design (start margin) and was only read.
- Q4 an attacker with one holder's key file and no database: can sign anything the key may sign until it is retired and revoked (the intended threat); cannot make the daemon do anything. Planting a file for adoption: P7-10.
- Q7 conformance of success and error answers to the schema for every operation was only read plus the author's checker test (green on Windows); the cluster test was not run.

## 6. Test quality

One-line mutants of my own against the ledgersign tests (Windows, in-memory store):

| Mutant | Result |
|---|---|
| trust file comparison in `load` off | killed (`TestAStoredSetThatDoesNotMatch...`) |
| clock skew check off | killed |
| `RecordLimit` rule off | killed |
| bound of a retired key always 1 | killed |
| retire ignores the bound in the event | killed |
| last-key check of the API (retire) off | killed |
| last-key check in the retire event step off | **survived** (the API check shadows it; with it off the step slices an empty list) |
| missing published trust file no longer an alarm | **survived** |
| memory store ignores the term | killed |
| key file base64 written to a log line | killed (three tests) |
| harmless log of a map of fingerprints | survived (as intended) |
| stale next key rule off, runner mutants | not run (mutant did not compile / needs PostgreSQL) |

Other points: the term guard test on the database store and the cluster test were not run by me; the scheduler tests use real sleeps and `eventually` with a five second bound (the follower test asserts a negative after 150 ms, a weak verdict); the kill tests stop at the engine's crash points; the secret search covers raw, hex (lower case) and base64 of the file and seed, which leaves the encodings of P7-07 unsearched, and the memory scan does not include the peer path.

## 7. Verdict

The collector that signs checkpoints may be built on this foundation once the following are fixed first:

1. P7-01: the retirement bound must be safe against a signing path (signing must stop using a key once its retired event exists; the retire commit must check the size under the guard). This is the one property the next step depends on and the current hand-over does not secure.
2. P7-04: an unopenable key file must be an alarm and be repaired, because the collector will meet the first failure of it when a leader changes.
3. P7-03: the loop and the unfinishable revocation, and the cleanup of a removed server, before keys are revoked in production.
4. P7-02 and P7-05 should be fixed or stated clearly in the operator documentation (the clock check is vacuous on one host; the stored state is as trusted as the database).

The rest (P7-06 to P7-15) may follow in the same step. The state machine, the term guard on the writes, the secret hygiene of logs, answers and rows, and the permission model held in everything I could run or read.

## 8. Not checked

PostgreSQL behaviour of the guard and of the runner, the two-server cluster test and the HA behaviour on either major (not run, see section 1); the web console; the verifier against a trust file produced here (only read, no run); behaviour on Windows hosts (key file permissions are not applied there); fuzzing; the real `Retry-After` and header behaviour of the HTTP layer for every error of every operation; the external signer and replacement of a lost key (out of scope).
