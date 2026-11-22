# Review of the console for table CSV export and import (phase 14a)

Reviewed: branch `dx-web` at 08e26ebfa5, the console commits since cb4afe3757 (855b88a0fe, de98f0f782, a8d952e806,
cec4997400, d34884da17, 7292fc19dd, 08e26ebfa5), against design `kingres/docs/design/data-export-import.md` (14a and console
sections), contract 0.19.0 and the server code in `kingres/control-plane` (`internal/ops/transfer.go`,
`internal/ops/transfer_import.go`, `internal/httpapi/handlers_transfer.go`, `internal/problem`).

All paths below are relative to `kingres/web` unless they start with `control-plane`.

## Summary

- No critical or high finding. The download and upload engines are sound on the main paths: every answer of a resumed
  download is checked, nothing short or changed is handed to the person, offsets are read from the server, and no
  file text reaches the DOM as markup (there is no `v-html` anywhere in `src`).
- Eight medium findings (DXW-01 to DXW-08), seven of them real defects on paths the tests and the mock do not exercise:
  an unbounded 409 loop, a cancel that cannot delete, a repeat import after an unknown outcome, the 256 MiB cap of reference
  tables, a download sink that fails in the wrong way, and gaps in the tests that let a wrong-upload regression through.
- Nine low findings (DXW-09 to DXW-17).
- Gates run clean: `vue-tsc` exit 0, eslint and prettier exit 0, vitest 97 files / 1580 tests passed, browser suite
  3 files / 49 tests passed (742 s).
- Mutants: 55 run, 36 killed (one by a hang), 19 survived. The survivors are listed at the end; the important ones guard real
  guarantees that no test checks (see DXW-06).

## Findings

### DXW-01 (medium) A 409 on the chunk is retried at once, without a pause or a limit

`src/lib/uploader.ts:256-263` (the `answer.conflict` branch of `send`), `src/api/endpoints.ts` (`uploadDataChunk`).

The conflict branch re-reads the offset and does `continue`. It does not count as a failure, does not sleep, and has no cap.
The real server answers `409 upload_offset_mismatch` with an unchanged `Upload-Offset` when another request still writes
the upload (`control-plane/internal/ops/transfer.go`, `OpenAppend` returning `ErrBusy`: "Another request is writing this
upload"). That is the normal state right after Pause then Resume, after the dialog was closed and reopened, and after a
chunk was abandoned by the browser while the server still reads its body. The mock never produces it (it answers 409 only
for a different offset), so no test sees it.

Scenario: the person presses Pause and at once Resume. The new PATCH meets the old handler: 409, same offset, the console asks
`getDataUpload`, sends the same 64 MiB slice again, and repeats in a tight loop until the server releases the writer. I
confirmed the loop with a probe: a server that always answers conflict with the same offset made the uploader call
`uploadDataChunk` 300 times with 0 sleeps and no stop. Each round may also push a large body that the server ignores.

Fix: count a conflict that does not advance the offset as a failure (use `failures`, `sleep` with backoff, and the
`maxFailures` cap). Also check that an answer moves the offset forward and not past the end (`answer.offset > from`, `<=
file.size`); a server that answered 204 with the same offset would loop in the same way. Add a mock switch for "busy writer".

### DXW-02 (medium) Cancel of an upload forgets it even when the server could not delete it

`src/lib/uploader.ts:331-345` (`cancel`), `src/components/transfers/UploadStatus.vue` (button in the password form),
`src/components/transfers/TransfersPanel.vue` (the list of tab uploads).

`cancel()` swallows every error of `deleteDataUpload` and then calls `onUpload(null)`, which removes the id from
`sessionStorage`. The phase becomes `cancelled` and the dialog returns to the options with no message. In the `password`
phase the session is gone, so the DELETE is certain to answer 401 (`keepSession` is set, so it is not even reported). The upload
stays on the server with its whole declared size reserved in the staging quota for up to 48 hours, and because the panel
lists only the uploads the tab remembers, the person has no way left to delete it.

Probe: an uploader in `password` phase, delete answering 401: after `cancel()` the phase is `cancelled`, `onUpload` was called
with the id and then `null`, and the mock still holds 1 upload.

Consequence: a "Cancel the upload" that the person believes frees 20 GiB leaves it reserved; the next upload may fail with
`transfer_quota_exceeded` ("The staging area is full") for two days.

Fix: if the delete fails with anything but 404, keep the id in the store (marked "deletion pending"), say so in the dialog
("The upload could not be deleted; sign in and delete it from the Transfers panel"), and in the password phase offer
"sign in and cancel". The comment "the server deletes an upload that gets no data for 48 hours" is true but is not what the
person is told. `cancel` should also ask for a confirmation when more than a few MiB were sent (it is a neutral button next to
Pause today).

### DXW-03 (medium) After an import whose outcome is unknown the same file can be imported again with one click

`src/components/transfers/ImportTableDialog.vue:136-150` (`startSending`), `:124` (`outcomeUnknown`), `:80`;
`src/lib/upload-store.ts`; `control-plane/internal/ops/transfer_import.go` (`importOnFail` keeps the upload).

After `import_outcome_unknown` the dialog correctly offers only Close. But the upload stays complete on the server (the server
keeps it so that "the import can be tried again") and stays in the tab's memory (`onDone` forgets it only on success).
Reopening the dialog and choosing the same file shows "This tab started to upload this file earlier. The upload is continued
from where the server has it." The person goes to Options and presses "Upload and import": `upload.start(existing)` reads the
upload, finds it complete, the watcher fires and `startImport()` runs a second import at once, without any mention that the
last import of this very file may already have loaded rows. The text the person was given ("check the table before you
repeat the import") is not repeated at the one place where repeating happens. The same holds after a successful import with
`keepUpload` (the upload is complete and remembered, so choosing the file again imports it again without a word).

Fix: when a task of an upload ends with `import_outcome_unknown`, mark the stored entry (`importOutcomeUnknown: true`, plus the
time) and, when the same file is chosen again, show a danger notice and require an explicit tick ("I checked the table")
before "Upload and import"; or delete the upload on that outcome so that a repeat needs a new upload. For `keepUpload`,
say in the notice that the file is already on the server and was imported.

### DXW-04 (medium) The size cap for a reference table is 256 MiB on the server; the console and the mock know only 2 GiB

`src/lib/transfer.ts` (`MAX_ATOMIC_BYTES`), `src/components/transfers/ImportFileStep.vue` (`refusal`),
`src/api/problem.ts:465-470` (`import_too_large`), `src/mocks/transfer.ts` (`importTable`);
server: `control-plane/internal/ops/transfer_import.go:299-303` (`MaxAtomicReferenceBytes = 256 MiB`), and `maxAtomic()`
(configurable by a flag, not exposed in `DataTransferSettings`).

The server lowers the cap to 256 MiB for a table of class `reference` and for any deployment that sets a smaller
`MaxAtomicBytes`. The check happens in `importTable`, after the whole file is uploaded. The console tells the person "at
most 2 GiB" in the file step, in the all-or-nothing notice and in the fixed text of `import_too_large`, accepts a 1.5 GiB file
for a reference table, uploads it (possibly for hours), and then shows "limited to 2 GiB of data. Split the file into parts of at
most 2 GiB" - a sentence that is false for that table and would send the person into the same refusal again. The design says
the console hides the action for reference tables; it does not (`TableTransferActions.vue`).

Fix: pass the table class to the dialog and use 256 MiB for `reference` in the file step and before the first byte; make the
text of `import_too_large` come from the server's detail (it already names the byte count) or give the problem entry
`withDetail: true`; expose the effective cap in `DataTransferSettings` (additive) so that a lowered flag is not a surprise;
teach the mock the same cap (a test would then catch this).

### DXW-05 (medium) A failing save sink is reported as a broken network, can duplicate bytes, and a failing close leaves the dialog stuck

`src/lib/downloader.ts:186` (`await o.sink.write`) inside the `try` of the read loop, `:190-196`, `:213`
(`await o.sink.close()`), `src/composables/useExportDownload.ts:36-66`.

1. `sink.write` runs inside the same `try` as `reader.read()`. A failure of the file (disk full, a revoked permission, a
   closed handle) is not an `ApiError`, so it is classified as a break (`broke = true`), and the download is resumed five
   times with a growing pause, then ends as `network`: "The connection broke and could not be resumed". Probe: a sink whose
   `write` always throws gave `{reason: 'network', received: 0, resumes: 5}` after 6 requests. The person is told to
   check the connection when the disk is full.
2. `MemorySink.write` adds the chunk and the size before it throws over 256 MiB, while `received` is not advanced; a retry
   would write the same bytes again. It cannot happen through `chooseSaveMethod`, but only the SHA-256 check would stop it.
3. `await o.sink.close()` is outside any `try`. `FileSystemWritableFileStream.close()` is where a full disk is reported,
   because the data is written to a swap file. Then `runDownload` rejects (probe: confirmed), although it promises
   "It never throws for a failed download". `useExportDownload.start` has no `try/catch`, so `state` stays `running` for ever
   (the button says "Cancel the download"), no message is shown, and the browser logs an unhandled rejection.

Fix: wrap `sink.write` and `sink.close` separately and return a new reason (`save`: "The file could not be written to the
place you chose (for example the disk is full). Nothing was saved."); do not resume on a sink error; make `start()`
catch anything and set `state = 'failed'`. Let `MemorySink` check the size before it appends.

### DXW-06 (medium) The tests let a wrong-import regression pass: 19 mutants survived

`tests/import-dialog-0-19.test.ts`, `tests/downloader-0-19.test.ts`, `tests/uploader-0-19.test.ts`,
`tests/transfers-panel-0-19.test.ts`.

The most serious survivors guard the question "can an upload be attached to the wrong import?":

- I2: removing `completeUpload.value = null` from `onChosen` (`ImportTableDialog.vue:80`) passes all tests. Without it the
  sequence "import fails, Change the options, Back, choose another file, Import again" sends `importRequest` with the id
  of the first file's upload: the dialog shows file B and imports file A. No test chooses a second file after a first
  upload completed. Today the line is present; nothing keeps it so.
- U4: the uploader takes `this.set({ offset: to })` (its own guess) instead of the offset in the answer, and all tests
  pass.
- D10: `useExportDownload` saves the memory blob whenever there is a memory sink, even after a failed download (it would
  hand over an empty or partial file). No test checks that a failed download saves nothing through the composable.
- D4, D6, D7, D13, D14 (overrun beyond the declared size, the first answer 206 with a wrong `Content-Range`, the size and
  length checks of a resumed answer, a body that is null) are not tested.
- I4 (the compressed flag reaches the uploader from the dialog), I7 (the upload is gone when the import starts, `not_found`),
  P3 (a 404 on deleting a stored upload), P4 (the subject of a busy task matters: any busy task disables every table), E4
  (an answer without `Upload-Offset`), M5 (the mock hides another role's upload), D11, D12 (the 256 MiB limit and the
  `link` fallback: no test chooses `link` at all), U8, U11, U12 survive.

Fix: add a test per item above; the first three first. Add a test that drives the dialog through two different files, and a
test of `useExportDownload` with a failing download and a memory sink that asserts `save` was not called.

### DXW-07 (medium) A password prompt exists for uploads only; a download that loses the session tells the person to sign in and offers no way to

`src/api/endpoints.ts` (`downloadDataExport`, `keepSession: true`), `src/lib/downloader.ts:35`, `:84-95`,
`src/lib/transfer.ts` (`describeDownloadFailure`, case `session`), `src/components/transfers/ExportDownload.vue`.

For a download the 401 is thrown with `keepSession`, so `reportSessionLost()` is not called and the console does not redirect.
The dialog then says "Your session ended ... Sign in again, then download again." but the page still believes the person is
signed in, there is no password prompt in this dialog, and the next normal request elsewhere is what finally sends the person
to the sign-in page. The header comment of `downloader.ts` says "The bytes held are kept"; `fail()` aborts the sink, so the
bytes are thrown away and "Try again" starts at byte 0. For a 20 GiB file that is a real cost.

Fix: for a download, let the 401 report the lost session (no `keepSession`), or reuse the password form of the uploader; fix the
comment; consider keeping the partial file and resuming with `Range` after the sign-in (the ETag check already protects it).

### DXW-08 (medium) The "link" fallback gives up every check the console makes everywhere else

`src/components/transfers/ExportDownload.vue` (`dl.method.value === 'link'`), `src/lib/downloader.ts:292-295`.

A browser without `showSaveFilePicker` (Firefox, Safari) and a file above 256 MiB gets a plain `<a href download>`. That is
what the design specified and the text is honest ("compare the SHA-256 of the saved file with the one shown"). But there is no
role gate (the link is a plain anchor, not a `GateButton`), no size or ETag check, no resume the console controls, and a session
that ended gives whatever the browser does with a 401 body. I did not test the behaviour of any browser. The text does not
say how to compute a SHA-256 of the saved file (for example `sha256sum` or `certutil -hashfile`). No test exercises this path.

Fix: show the command for the person's system next to the SHA-256; make the link appear only when `session.can('downloadDataExport')`;
add a test for `chooseSaveMethod` at 256 MiB plus one byte without the picker (D12 survived).

### DXW-09 (low) 400 with an `Upload-Offset` header is treated as final

`src/lib/uploader.ts:222-229`, `src/api/endpoints.ts` (`uploadDataChunk`); server `UploadChunk`: a body shorter than its
`Content-Length` is `400 bad_request` with the header `Upload-Offset: <kept>`.

The console treats only 409 with the header as "the server says where it is". A 400 carrying the header goes to `stopOn` and
becomes `rejected`: "The server refused a part of the file. Start the upload again.", with no Resume button
(`canResume` is true only for `network` and `other`). It happens when a proxy cuts the request body but still delivers the
server's answer. Fix: treat a 400 with a valid `Upload-Offset` like a break (take the offset, count a failure, continue).
The mock should produce this answer.

### DXW-10 (low) A download that arrived whole but whose stream then failed is reported as a failed range

`src/lib/downloader.ts:198-200`. If every byte arrived and the reader then threw (a reset at the very end), `broke` is true and
`received === expectedSize`; the code asks for `Range: bytes=<size>-`, the server answers 416, and the outcome is `range`
and the sink is aborted, throwing a whole file away. Fix: test `received === o.expectedSize` before `broke`.

### DXW-11 (low) The streaming body of a download is not aborted when the session ends

`src/api/client.ts:97`. `inFlight.delete(controller)` runs when `fetch` resolves, which is when the headers arrive. A download
that streams for minutes is therefore no longer in the set that `abortInFlightRequests()` cancels on sign-out or a change of
user. The bytes were already authorised for the first person, so the harm is small (a file keeps writing to the first person's
chosen file after a sign-out), but the stated rule "a late answer can never reach the next user" does not hold for
streams. Fix: keep the controller until the body ends, or let `downloader.ts` register its own.

### DXW-12 (low) Resume identity of an upload is weak

`src/lib/upload-store.ts` (`findStoredUpload`), `src/lib/uploader.ts` (`read`).

An upload is continued when the name, size, modification time and table match, and the server reports the same size. The content
is never compared. A file replaced by another of the same size and the same modification time (a re-export with equal size,
a tool that preserves the time) would have its tail appended to the head of the old file. The console never gives the
server a SHA-256 (by design, DXR-27), and the SHA-256 it shows after the upload has nothing to be compared with in the console.
Cheap protection: the preview already reads the first 64 KiB; store a hash of the first and last 64 KiB with the entry and compare.
Also: the store is not keyed by user. After another administrator signs in in the same tab, the panel lists the previous person's
uploads (file name, table); "Delete upload" answers 404 and forgets the row. Key the entries by user name.

### DXW-13 (low) The default header option "ignore the first line" can drop a row of a file that cannot be previewed

`src/lib/csv-guess.ts:154` (`header: true` for an empty or compressed file), `src/components/transfers/ImportTableDialog.vue:92-95`.

For a gzip file no preview is shown and the guess answers `header: true`, so the dialog preselects "Has column names; ignore
them". A headerless gzip file loses its first row. The import is "all or nothing" but the row count is not compared with
anything the person knows. Fix: for a compressed file leave the first-line option unselected and require a choice, with a sentence
that explains why.

### DXW-14 (low) The spreadsheet guard is explained honestly in the dialog but overclaimed in the result

`src/components/transfers/ExportResult.vue` (`export-lossy`), `src/components/transfers/ExportTableDialog.vue`.

The dialog says the guard "is a convenience, not a security feature" and that a value of another kind (json, an array) is not
changed. The result then says "The values that begin with a formula character were changed with an apostrophe", which reads as
"all". The flag also comes from the form's state (`:spreadsheet-safe="form.spreadsheetSafe"`), not from the server: the server
keeps `spreadsheetSafe` in the report of the file but `DataExport` does not carry it, so when the person comes back through the
Transfers panel there is no spreadsheet warning at all for a file that was written with the guard off. Fix: expose the flag in
`DataExport` (additive), show the warning in the panel's download dialog, and repeat the "not for json and arrays" limit in the result.

### DXW-15 (low) The mock differs from the server in places the tests do not look

`src/mocks/transfer.ts` against `control-plane/internal/httpapi/handlers_transfer.go` and `internal/ops/transfer.go`. Compared and
found the same: status codes of every problem code (403, 409, 413, 422), the 409 with `Upload-Offset`, 416 with `Content-Range: bytes */size`,
410 with code `not_found`, the 64 KiB rule, creator-only reads and the admin delete of any export, TLS and setting guards, quota, ETag
(the SHA-256 in quotes). Different:

- An unparsable or multi-range `Range` header: the server serves the whole file with 200 (`byteRange` returns `ok=false`); the mock
  answers 416.
- A chunk for an upload that is already complete: the server answers 404 (`rec.State != receiving`); the mock answers 409.
- A cut chunk: the server answers 400 with `Upload-Offset` (DXW-09) and 409 for a busy writer (DXW-01); the mock only drops the
  connection.
- The import cap for a reference table (DXW-04).
- `Content-Type` charset: the server uses the export's encoding; the mock always says UTF-8.
- The quota reserve: the server adds the sealed-form overhead; the mock reserves the plain size.

### DXW-16 (low) Accessibility: names, announcements, focus, destructive actions

`src/components/transfers/TransfersPanel.vue:151-165`, `:198`, `src/components/transfers/ExportDownload.vue`,
`src/components/transfers/UploadStatus.vue`, `src/components/transfers/ImportTableDialog.vue`.

- Every row has buttons named only "Download", "Delete" and "Delete upload"; a screen-reader user hears the same name N times.
  Add `aria-label` with the table name (and the file name for uploads).
- The progress bars of the upload and the download are `role="progressbar"` without any announcement; `TaskProgress` announces every
  10 percent through `useAnnounce`, these do not. Failure and success are announced (`role="alert"` / `status`).
- The focused button is replaced by another (`Download file` to `Cancel the download`; the buttons of the import steps use `v-if`),
  so focus falls to the page. This I read in the code and did not test with a keyboard.
- "Cancel the upload" deletes a staged file with one click and without a confirmation, in the neutral style (DXW-02).
- On the good side: `data-autofocus` on the password field and on the file input, the aria-disabled gates with a reason, the role
  `alert` for the outcome-unknown notice, and the axe suite passed (49 browser tests).

### DXW-17 (low) The panel lists tab uploads from a one-time read

`src/components/transfers/TransfersPanel.vue:49`: `const uploads = ref(listStoredUploads())` is read once; an upload finished or forgotten
by a dialog opened later does not leave the list until a reload. "Delete upload" then answers 404 and is forgotten silently,
which is handled, but the list is stale.

## Verified as correct

Download integrity:
- Resume sends `Range: bytes=<received>-` only when `received > 0`; a 206 must have `Content-Range` starting at `received` and
  the declared total; `Content-Length` must equal the rest; the first answer must have the declared total (206) or length (200).
  Mutants on the ETag check, the first-byte check, the SHA-256 check and the "skip bytes when Range is ignored" path were killed.
- A server that ignores `Range` (200 on resume) is handled by skipping exactly `received` bytes; more bytes than declared stop the
  download as `changed`.
- A short stream, with or without an error, is resumed and then reported as `short` or `network` with the counts, and the sink is
  aborted; the memory path hands the file over only after the SHA-256 matched (`useExportDownload`, line 62); 256 MiB is the
  limit of the memory path; larger files use the picker or the link.
- 410, 404, 403, 416, 401 are separate outcomes with true sentences; a cancel stops and says nothing was saved.
- The file name offered to the picker is built from identifiers only (`exportFileName`); the link is built from a digits-only id.

Upload integrity:
- The offset shown and used is the server's: read after a break, a conflict, a failure, a pause and a sign-in. A 409 with
  `Upload-Offset` is a conflict, not an error. Chunks are `Blob.slice` bodies, one at a time, aligned to the server's `chunkAlign`
  and cut to `chunkBytesMax`; the last chunk may be shorter.
- The 2 GiB cap (not for gzip) and the largest staged file are enforced before the first byte; an empty file is refused; a
  remembered upload of another size is never continued.
- A 401 between or in a chunk stops at `password` phase and continues from the server's offset after sign-in; the session is
  checked between chunks. `keepSession` stops the redirect for the upload calls only.
- The upload that the import uses is `completeUpload`, set from the snapshot's id when the phase is `complete` and the step is
  `send`; a new file clears it (but see DXW-06, I2). The stored entry is validated on read (digits-only id, finite size).

Security in the browser:
- No `v-html`, `innerHTML` or similar anywhere in `src`. File names, header cells, CSV cells, task details, steps and error
  messages are drawn through `SafeName` (hidden and bidirectional characters made visible). Mutant Z1 (a `v-html` in the preview) was
  killed by the hostile-text test.
- `GateButton`, `session.can` and the settings page read the role from the contract table (`operations.generated.ts`); the server
  enforces `kingres_admin` on every one of the ten operations and re-checks the role at each step. The console gate reasons
  ("turned off", "waiting for the operation") are hints; the server decides (403 `data_transfer_disabled`, 409
  `operation_in_progress`).
- Creator-only files: the mock and the server agree (another role sees 404; any administrator may delete). The panel says "Only your own
  files are listed" and does not claim more.
- The import's unknown outcome is shown with `role="alert"`, offers no "Change the options" and no repeat button in the dialog;
  failed tasks show the server's whitelisted text through `SafeName`. (The repeat through a reopened dialog is DXW-03.)
- The settings group sends only the fields that changed, sizes in bytes only when the GiB field changed, and its limits match
  `control-plane/internal/transfer/settings.go` (retention 1-168, concurrent 1-16, size at least 1 MiB).

## Commands run

- `npm ci`, `npm run typecheck` (exit 0), `npm run lint` (exit 0), `npx vitest run` (97 files, 1580 tests, exit 0),
  `npm run test:browser` (3 files, 49 tests, exit 0, 742 s).
- Four probe tests of my own (a closing sink that throws, a writing sink that throws, a persistent 409, a cancel with a refused
  delete) confirmed DXW-01, DXW-02 and DXW-05; the probe file was deleted.

## Mutants

Each was applied to one file in the scratch worktree, run against the named test files, and reverted. K = killed, S = survived.

| Id | File | Change | Tests | Result |
|----|------|--------|-------|--------|
| D1 | lib/downloader.ts | drop the ETag check | downloader | K |
| D2 | lib/downloader.ts | resume accepts any `Content-Range` start | downloader | K |
| D3 | lib/downloader.ts | drop the SHA-256 mismatch failure | downloader | K |
| D4 | lib/downloader.ts | drop the "more bytes than declared" check | downloader | S |
| D5 | lib/downloader.ts | `skip = 0` when Range is ignored | downloader | K |
| D6 | lib/downloader.ts | first 206: drop `first` and `size` checks | downloader | S |
| D7 | lib/downloader.ts | resumed 206: drop the total size check | downloader | S |
| D8 | lib/downloader.ts | accept a stream that ended short | downloader | K |
| D9 | lib/downloader.ts | 410 no longer `gone` | downloader | K |
| D10 | composables/useExportDownload.ts | save memory blob even after a failure | downloader, panel, export dialog | S |
| D11 | lib/downloader.ts | `MemorySink` limit off | downloader | S |
| D12 | lib/downloader.ts | no `link` fallback, always memory | downloader, panel, export dialog | S |
| D13 | lib/downloader.ts | resumed 206: drop the length check | downloader | S |
| D14 | lib/downloader.ts | drop the null body check | downloader | S |
| U1 | lib/uploader.ts | drop the 2 GiB cap | uploader | K |
| U2 | lib/uploader.ts | drop the size-differs check | uploader | K |
| U3 | lib/uploader.ts | cancel does not delete | uploader | K (hang, killed by timeout) |
| U4 | lib/uploader.ts | offset from own guess, not the answer | uploader | S |
| U5 | lib/uploader.ts | no 64 KiB alignment | uploader | K |
| U6 | lib/uploader.ts | 401 no longer stops for the password | uploader | K |
| U7 | lib/uploader.ts | no session check between chunks | uploader | K |
| U8 | lib/uploader.ts | failures not reset on progress | uploader | S |
| U9 | lib/uploader.ts | empty file accepted | uploader | K |
| U10 | lib/uploader.ts | largest staged file not enforced | uploader | K |
| U11 | lib/uploader.ts | no check that the upload is complete at the end | uploader | S |
| U12 | lib/uploader.ts | lost/deleted state not treated as gone | uploader | S |
| S1 | lib/upload-store.ts | table not part of the key | csv-guess, import dialog, uploader | K |
| S2 | lib/upload-store.ts | size not part of the key | same | K |
| S3 | lib/upload-store.ts | modification time not part of the key | same | K |
| S4 | lib/upload-store.ts | id not validated on read | same | K |
| I1 | transfers/ImportTableDialog.vue | "Change the options" shown after unknown outcome | import dialog | K |
| I2 | transfers/ImportTableDialog.vue | new file does not clear `completeUpload` | import dialog | S |
| I3 | transfers/ImportTableDialog.vue | "Import again" uploads again | import dialog | K |
| I4 | transfers/ImportTableDialog.vue | compressed flag not passed | import dialog | S |
| I5 | lib/transfer-form.ts | BOM sent for non-UTF8 | export dialog, csv-guess | K |
| I6 | transfers/ExportResult.vue | spreadsheet note always shown | export dialog | K |
| I7 | transfers/ImportTableDialog.vue | gone upload not reset | import dialog | S |
| I8 | transfers/ImportTableDialog.vue | unknown-outcome notice hidden | import dialog | K |
| P1 | transfers/TransfersPanel.vue | download enabled for a file not ready | panel | K |
| P2 | transfers/TransfersPanel.vue | no per-database filter | panel | K |
| P3 | transfers/TransfersPanel.vue | any error of delete treated as 404 | panel | S |
| P4 | composables/useTransferGate.ts | any busy task blocks every table | export dialog | S |
| E1 | api/endpoints.ts | 409 not treated as conflict | uploader, import dialog | K |
| E2 | api/endpoints.ts | no `Range` header sent | downloader, panel, export dialog | K |
| E3 | api/client.ts | `keepSession` ignored | uploader, import dialog | K |
| E4 | api/endpoints.ts | no error when `Upload-Offset` is missing | uploader | S |
| M1 | mocks/transfer.ts | mock accepts any offset | uploader, mock | K |
| M2 | mocks/transfer.ts | mock drops the 64 KiB rule | uploader, mock | K |
| M3 | mocks/transfer.ts | mock drops 416 | mock | K |
| M4 | mocks/transfer.ts | mock shows another role's export | panel, mock | K |
| M5 | mocks/transfer.ts | mock shows another role's upload | import dialog, mock | S |
| Z1 | transfers/CsvPreview.vue | cell through `v-html` | import dialog | K |

55 mutants: 36 killed, 19 survived. Equivalent or low value: U12 (the next read fails anyway), U8 (cap of retries), D11 (cannot be
reached through `chooseSaveMethod`). The others are listed in DXW-06.

The worktree is clean after the run (no tracked changes); the mutants and the probe file were removed.


## Fixes

Fixed on branch `dx-web` after the review, with the tip of `dx-cp` (97976b3c44) merged in. Each fix has a test; the tests are
`tests/review-dxw-lib-0-19.test.ts` and `tests/review-dxw-ui-0-19.test.ts` unless another file is named.

| Finding | What was done | Tests |
|---|---|---|
| DXW-01 | A conflict, a 400 that carries the offset, and a 204 that does not advance the offset (or goes past the end) are one case: the offset is read again, and when it did not move the failure counts, the uploader waits with a growing pause (1, 2, 4 ... s up to 8 s) and stops as `busy` after `maxFailures` (5), keeping the upload so that Resume goes on. A failure resets when the offset moves. The mock has a busy writer (`busyWriterChunks`, `?transfer=busy`): 409 with the offset unchanged. | busy writer stops with the cap and the pauses; the mock's two 409s then success; a 204 that does not advance; failures in a row only |
| DXW-02 | `cancel()` forgets the upload only when the delete worked or answered 404. Otherwise `deletePending` is set, the id stays in the tab's list, the options step says so, and the Transfers panel deletes it (a 401 there sends the person to sign in; the entry is kept). The password phase says that cancelling cannot delete. A cancel with more than 4 MiB sent asks first (a second click deletes). | refused delete keeps it; 404 and success forget it; the confirmation; the password text; panel keeps the entry on a refused delete |
| DXW-03 | The store marks an upload `imported: 'unknown'` after `import_outcome_unknown` and `imported: 'done'` after a success that kept the file. Choosing the same file again shows a danger notice and needs a tick ("I checked the table and I want to import this file again") before "Upload and import". | reopen with the same file after an unknown outcome: notice, no import without the tick, import with it; the kept upload is marked |
| DXW-04 | The dialog knows the cap of the table class (`atomicCapFor`: 2 GiB, 256 MiB for `reference`) in the file step, before the first byte (uploader), in the all-or-nothing text; the text of `import_too_large` is general and adds the server's detail (the byte count). The mock refuses a larger file for a reference table. The effective cap is not in `DataTransferSettings`: noted for the contract in `dx/web-contract-notes.txt`. | the refusal at 300 MiB and the accepted 256 MiB for `countries`; the mock's refusal |
| DXW-05 | A failing write or close is the failure `save` ("could not be written to the place you chose ... Nothing was saved"), is not resumed, and the file is thrown away; `MemorySink` checks the size before it keeps a chunk; `useExportDownload` catches anything and ends in `failed`. | a failing write (one request, aborted), a failing close, the memory limit, the composable with a failing sink |
| DXW-06 | Tests for the surviving mutants: I2, U4, D10, D4, D6, D7, D11, D12, D13, D14, I4, I7, P3, P4, E4, M5, U8, U11, U12, and for every fix above. 37 mutants of the fixed code were run (see below). | the two files above |
| DXW-07 | A 401 during a download no longer aborts the sink: the outcome is `session`, the bytes are kept, the dialog shows the password form, and after the sign-in the download goes on from the kept byte (`startAt`). The comment of `downloader.ts` is true now. | a run that stops at byte 100 and a second run from it makes the whole file and the SHA-256 matches; the composable offers `resume`; the component asks for the password and ends done |
| DXW-08 | The link fallback is shown only to a role that may download, shows the size and the SHA-256 and says how to compute it (`sha256sum`, `shasum -a 256`, `certutil -hashfile`). | the text, the href, the viewer sees no link; `chooseSaveMethod` at 256 MiB and one byte more |
| DXW-09 | A 400 with a valid `Upload-Offset` is an answer about the offset (as the 409). The mock can produce it (`cutAnswers400`, `?transfer=cut400`). | the endpoint returns it; a 400 without the header is still a refusal |
| DXW-10 | `received === expectedSize` is tested before `broke`: a stream that fails after the last byte is complete (one request). | one request, complete, verified |
| DXW-11 | `useExportDownload` registers with `registerServerStateReset`: a sign-out or a change of user aborts the request and releases the file; the dialog going away aborts the kept sink. | a running download ends `cancelled` on `resetServerState()` |
| DXW-12 | The store is keyed by the person (`user`) and lists, continues and deletes only that person's uploads; the entry holds a fingerprint of the first and last 64 KiB of the file, compared when both exist. | another person's entry is not listed or found; same name, size and time with other content (head, and tail beyond 64 KiB) is not continued |
| DXW-13 | A compressed or empty file has no default for its first line: the select shows "Choose ...", and the upload does not start until the person chose. | the message, no upload, then the upload after the choice |
| DXW-14 | `DataExport` has no `spreadsheetSafe`, so it cannot be read back (a contract note is in `dx/web-contract-notes.txt`). The result text says that a header cell too is changed, that json and arrays are not, that it is not a security feature, and to import with the first line ignored; the panel's download dialog warns that it does not know the option. | text of the result and of the dialog (export dialog tests) |
| DXW-15 | The mock now matches the server: an unparsable or multiple `Range` is the whole file (200), a chunk for a complete upload is 404, a cut chunk is a 400 with the offset (switch), a busy writer is a 409 with the offset unchanged (switch), the charset of the download is the encoding of the export, the import cap of a reference table, `skipBom` with another encoding is a 400 on `skipBom`, the step names of the task. The quota reserve overhead is not modelled. | the mock tests |
| DXW-16 | The row buttons of the panel have names with the table or file name; the progress of the upload and of the download is read out every 10 percent; the cancel asks first; the focus is not changed in this round. | names are used by the panel tests |
| DXW-17 | The list of the tab's uploads reads the store reactively (`storeVersion`), so a dialog opened later changes it at once. | an upload remembered after the panel was drawn appears |

Contract changes followed (dx-cp 97976b3c44): the steps of `table.import` are `Check the upload`, `Check the table`, `Load rows`,
`Confirm the commit`, `Clean up` (the console never matched on a step name; the mock has them); `skipBom` is sent as false for an
encoding other than UTF8 (the server answers 400 otherwise); a task failure `copy_interrupted` is shown with the sentence of the server
and the note "Nothing was stored" and can be repeated, `import_outcome_unknown` stays the rare case with no repeat; a 403 `forbidden` for
a role that may not import is shown by the general text of the code.

### Mutants of the fixed code

37 mutants were run against the fixed console (a defect put in by hand, the tests that must notice it, the file restored): all 37 were
killed (new ones for DXW-01 to DXW-17 and for the survivors D4, D6, D7, D10, D11, D12, D13,
D14, E4, I2, I4, I7, M5, P3, P4, U4, U8, U11, U12). The script is `dx/mutants2.cjs` in the scratch area of the
writer. The first run of the new set killed 36 of 37, the 37th (the session failure of an `open` that aborts the sink) had been
written for a line that prettier had wrapped, and was killed when it was applied to the right line.
