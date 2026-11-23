# Independent review: console of phase 9, increment 9.7 (commit bab67e6b2e, contract 0.14.0)

## Scope and method

Reviewed commit bab67e6b2e on branch web-15b (parent 41b556117a): the table page, the dialogs of the table lifecycle
(distribute, undistribute, alter, create/attach/detach/drop partition), the preview composable, the task panel, the 9.7 mock
and its tests. Read the 9.7 and console sections of shard-phase9.md, the eight 0.14.0 operations and schemas in openapi.yaml, and
the whole diff. Built a private worktree, ran `npm ci`, typecheck, lint (eslint + prettier), the full vitest suite and the browser
tests, and ran 12 mutants (11 valid) on the two table test files.

Results of the runs:
- typecheck: clean. lint: clean (0 warnings, prettier clean).
- vitest: 92 files, 1513 tests, all pass.
- browser tests (`npm run test:browser`): 2 of 3 files, 44 of 46 tests passed; the run hit my own 550 s timeout while the third file
  was running (worker killed with exit 143). So the browser suite is NOT confirmed green by this review; it needs one run
  with a longer limit.

## Findings

### C97R-001 (medium) Undistribute and alter start with one click; the design asks for type-to-confirm and a danger warning
Files: kingres/web/src/components/tables/UndistributeTableDialog.vue (whole dialog: no `type-to-confirm`, no `danger`);
kingres/web/src/components/tables/AlterDistributionDialog.vue (same).
The design (Web console, Dialogs) says undistribute has "a warning in tone-danger: The table will live on one server. Its data must
fit there" and "Type-to-confirm", and alter has "Type-to-confirm of the table name when data moves". The dialogs show the preview
(good) but the confirm button is a plain primary button. Scenario: an administrator opens Undistribute on a multi-terabyte table,
leaves the default target group 0 (the map group) and presses the button once; hours of copy start on a server that cannot hold it
(the only guard is the server-side free space check). A mistaken "Change the distribution" with cascade rebuilds every colocated
table, and in blocking mode writers wait for the whole copy. Fix: add `TypeToConfirm` of the table name when rows move
(undistribute: whenever rows > 0), use `danger`, and add the tone-danger sentence.

### C97R-002 (medium) A cancelled or failed task is declared "as it was before" without evidence
Files: kingres/web/src/components/tables/TableTasksPanel.vue:100 (and the predicate at :53);
kingres/web/src/components/TaskActionDialog.vue (`cancelledText`, passed by every dialog: "The table is as it was before", "No
partition was created", "The partition is as it was"); the generic line in TableTasksPanel "The table stays as it was unless the
message says what is left to do".
In the control plane the undo of a cancelled online operation is a compensation (`tableOnFail`, ops/tablelife.go:206-219): the task
is recorded as failed with code `cancelled` first and the extension's `Cancel` runs afterwards; if that fails the control plane only
logs a warning. The console reads only `error.code === 'cancelled'` and then states a fact that no field of the contract carries.
Scenario: the operator cancels a copy, the compensation cannot reach one node, and the page says "The table is as it was before"
while a shadow table and capture trigger remain (the design wants the dialog to "say which server will be cleaned when it
returns"). The same holds for the generic sentence on any other failure, such as a verification failure. Fix: for a cancelled task say
"Cancelled; the control plane undoes the change" and show `error.message` and `task.detail` as sent; claim "as it was" only when the
table's own operation marker is gone, or word it as an expectation.

### C97R-003 (medium) The mock has no blocked/paused, verification-failed or pending table task, so those UI paths are untested
Files: kingres/web/src/mocks/tables.ts (`startTask`; only the switches `fail:space`, `fail:copy`, `slow`, `busy`),
kingres/web/tests/tables-0-14-ui.test.ts.
The design promised `blockme` (waits with blockers) and `diffme` (verification fails, old copy kept). A search for `paused` and
`verification` in the new tests and the mock finds nothing. The real engine pauses a table task (tablelife.go:424 and :495) and
records blockers in `detail`. So the blocked state, Cancel of a paused task and the verification_failed wording have no test, and the
tests pass on a mock that knows only running, done and failed. The mock cancel also goes straight to failed with no compensation
delay, which hides C97R-002. Fix: add `blockme` and `diffme`, a cancel that shows cancelled and then settles, and tests for paused
with blockers, cancel while paused and the verification message.

### C97R-004 (medium, test quality) Mutant survived: the start button may be enabled while the preview is loading
File: kingres/web/src/composables/useTableChange.ts (`holdsStart`, line `if (loading.value) return true`).
Mutant M3 (return false while loading) passed all 58 table tests. The rule "no start before the preview of the current request is
in" (including the 250 ms debounce window after an edit) is the core of preview-then-confirm and nothing asserts it. Scenario of the
regression: an admin edits the mode or the target and clicks confirm inside the debounce window; the change starts against a request
whose blockers were never shown. Fix: a test that changes a field and asserts the confirm button is disabled until the new preview
is shown (use a delayed preview in the mock), and one for the first complete request.

### C97R-005 (low) Radio groups have no `name`
File: kingres/web/src/components/tables/TableChangeOptions.vue:51 and :70 (the mode and views radios).
Vue `v-model` on radios does not add a `name`; each radio is its own group for the browser, so the arrow keys do not move through the
options and every radio is a tab stop, unlike a native group. Fix: a unique `name` per fieldset.

### C97R-006 (low, test quality) Mutant survived: the 57-byte limit counted in characters
File: kingres/web/src/lib/tables.ts (`utf8Length`). Mutant M8 (`text.length`) passed. A 40-character name of 2-byte letters (80
bytes) would be accepted by the dialog and refused by the server with a field error (a wasted round trip, no data loss). Fix: a UI
test with a multi-byte name.

### C97R-007 (low) The "held" gate looks only at running and paused tasks among the newest 50
Files: kingres/web/src/lib/tables.ts (`runningTaskOfTable`), kingres/web/src/pages/TableDetailPage.vue (`useTasks(50)`).
A `pending` task (accepted, not yet running) does not hold the buttons, and a task older than the 50 newest tasks of the cluster is
not seen. The server stays the authority (409 `operation_in_progress`, shown by TableChangeRefusal), so this is only a missed early
hint. Fix: include `pending`, and filter the task request by subject if the contract allows it.

## Checked and found correct

- Bodies and parameters match the contract: undistribute sends only non-default query parameters (cascadeViaForeignKeys,
  targetGroupId, mode, dependentViews) on DELETE; alter sends only changed parts, with `colocateWith` as a TableRef or
  `default`/`none`, and `cascadeToColocated`; distribute sends `colocateWith: null`, optional `bucketCount` (only with a new group,
  1 to 4096), `mode`, `dependentViews`; create sends `name` (UTF-8 bytes checked), trimmed `bound`, `ifNotExists` only when set;
  attach sends a TableRef and `bound`; detach sends no body (allowed); drop has no body. Preview bodies use the `kind` plus
  `distribute`/`undistribute`/`alter`/`partition` shapes of TableChangeRequest. Generated types are used, so drift fails typecheck.
- Idempotency keys: the 0.14.0 operations define none; a double submit is stopped by the dialog (button disabled while starting) and
  by the server (409).
- Preview-then-confirm: every start dialog fetches `previewTableChange` and holds the button while the preview is stale or lists a
  blocker (M1, M4, M9 killed; the loading case is C97R-004). A server that cannot preview (422 `unsupported_operation`) lets the
  change start and the server's `table_not_eligible` blockers are then listed. Drop needs the typed partition name and a danger
  warning (M2 killed). Detach says no row moves and is stopped by the preview when foreign keys exist.
- Role gates: every action button is a `GateButton` with its operation id (M7 killed); the mock applies `canCall` per operation, so
  tests do not rely on a client-only gate; server refusals (403, 409, 422) are shown. Cancel needs `cancelTask`.
- States: running and paused tasks hold the page buttons; failed tasks show the server message and step in `role=alert`; cancelled
  tasks are shown as information, not failure (M6, M11 killed; wording issue is C97R-002); an old failure after a later success is
  dropped; a task past its switch shows Cancel disabled with the reason. Hostile names go through SafeName.
- Accessibility: labelled fields, focusable `aria-disabled` gate buttons with the reason, `role=alert` for blockers, `role=status`
  for loading; axe tests for the new pages exist and pass in vitest (the browser axe file was among the passing files).
- Mock: validates partition name length and bound, 404 for a missing partition, 409 `already_exists` and `operation_in_progress`,
  422 `table_not_eligible` with `blockers`, and a switch point after which the task is not cancellable, which mirrors the real
  `tableCancellable` rule.

## Mutants run (two table test files, 58 tests; every mutant restored)

| Id | Change | Result |
|---|---|---|
| M1 | TaskActionDialog: confirm button ignores `confirmDisabled` | killed (3 tests) |
| M2 | PartitionActionDialog: no type-to-confirm on drop | killed |
| M3 | useTableChange: `holdsStart` false while loading | SURVIVED (C97R-004) |
| M4 | `previewBlocked` always false | killed (3 tests) |
| M5 | Undistribute query drops `targetGroupId` | killed |
| M6 | TableTasksPanel: every failed task shown as cancelled | killed |
| M7 | Drop button gated by `getTable` instead of `dropTablePartition` | killed |
| M8 | `utf8Length` counts characters | SURVIVED (C97R-006) |
| M9 | `stale` always false | killed |
| M10 | Alter: cascade no longer forces blocking mode | killed |
| M11 | Dialog: cancelled code never recognised | killed |
| M12 | Page `heldReason` removed (the sed made invalid code; not meaningful, not counted) | invalid |

The worktree was clean after the mutants (git status empty).

## Fixes

All seven findings are fixed on branch web-15b. The tests of the fixes are in `kingres/web/tests/tables-0-14-review.test.ts` and in the
changed tests of `tables-0-14-ui.test.ts`.

- **C97R-001.** The undistribute dialog is a `danger` dialog with the sentence "The table will live on one server. Its data must fit
  there." (tone-danger) and asks for the name of the table to be typed, unless the table is known to have no rows. The alter dialog asks for
  the name when rows move (a change of the column or the colocation, or a bucket count that is not a multiple of the old one) and shows a
  danger sentence; a change that moves no row needs no typing.
- **C97R-002.** No dialog and no panel says that the table is "as it was". The text after a cancel (`CANCELLED_TEXT`) says that the control
  plane undoes what the change had begun and that what it reports is shown with it. The dialog reads the cancelled task again every 2
  seconds for 2 minutes, so the detail the control plane sets later (the undo done, or a server that is cleaned up when it returns) appears.
  The page panel shows `task.detail` for a cancelled and for a failed task, and a verification failure says that the old copy was kept and
  that an administrator has to look at it.
- **C97R-003.** The mock has the switches `blockme` (the task pauses at its switch with the blockers in its detail), `diffme`
  (`verification_failed` after the switch), `pending` (accepted and not started), `undo:<ms>` and `undo:fail` (the undo of a cancel is reported
  after a delay, or names a server that could not be reached). A paused table task can be cancelled. Tests: paused with blockers and Cancel
  while paused, the undo that settles, the undo that failed on one server, the verification failure in the dialog and on the page.
- **C97R-004.** A test holds the preview with a gate: the start button is disabled while the first preview loads, inside the delay after a
  change and while the new preview loads. The mutant M3 is killed.
- **C97R-005.** The radios of the mode, of the views and of the method of distribution have one `name` per group (`useId`). The test checks
  the names. (The arrow keys are the browser's behaviour for radios of one name; jsdom does not move the focus, so the test asserts the name.)
- **C97R-006.** A test with 30 two-byte letters (60 bytes) is refused and 28 (56 bytes) is accepted. The mutant M8 is killed.
- **C97R-007.** A `pending` task holds the buttons. The page reads the newest 200 tasks (`TABLE_TASKS_LIMIT`) instead of 50; the contract has
  no filter by subject, so an older task is still not seen, and the comment in the code and the README say that the 409
  `operation_in_progress` of the server is the authority.

Mutants run against the new tests (every one killed, every one restored): M3 (start enabled while loading), M8 (characters, not bytes), no
`name` on the radios of the mode, no type-to-confirm on undistribute, alter that never asks, a cancelled text that claims "as it was", a
pending task that does not hold the buttons, no re-read of a cancelled task.
