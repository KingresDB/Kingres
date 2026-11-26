<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Independent review: console of phase 12 (branch p12-web, tip 9a78cf18d5)

Scope: commits c7474b8950, 09220882e9, 2750ea1a66, 1897f91e64, 9a78cf18d5 (kingres/web). Compared with the contract
(kingres/api/openapi.yaml, 0.18.0) and the control-plane code on the same branch (internal/httpapi/handlers_analytics.go,
internal/ops/partition_policy.go, tablestorage.go, storage_task.go, maintain.go).

Gates run on the tip: typecheck clean, lint (eslint + prettier) clean, vitest 95 files / 1685 tests pass, browser suite
4 files / 48 tests pass (780 s).

No critical finding. Two high findings (the data-loss preview is false, and Save is live over a failed load).

## Findings

### P12W1 (high) The delete warning says "0 B" because the real plan carries no sizes
- src/components/tables/RunPolicyDialog.vue:34-36,81-84; src/components/tables/PartitionPolicyDialog.vue:120-131,512-517.
- The server planPartitionPolicy (ops/partition_policy.go, PlanPartitionPolicy) scans only seq, kind, partition, range and reason;
  bytesBefore and bytesAfter are always null in a plan (only run actions read from the run table have them). The console sums
  `bytesBefore ?? 0`. The mock (src/mocks/analytics.ts:874,886) fills bytesBefore, so every test sees a real number.
- Scenario (reproduced with a plan of one drop action with null bytes): Run now shows "The run drops 1 partition and deletes 0 B of
  data. This cannot be undone." The policy dialog says "Dropping 1 partition deletes 0 B of data". An administrator reads this as
  "nothing of value" and confirms. The design asks for sizes in the preview.
- Fix: when no drop action has a size, say "the size is not known" (never 0); show the partition names and ranges as the main
  information; or take the sizes from listTablePartitions (sizeBytes) and join by name. Make the mock return null bytes for plan
  actions, as the server does, and add a test. Better: the server fills the sizes of the plan (contract change).

### P12W2 (high) "Save policy" stays enabled when the stored policy could not be read, and saves the defaults
- src/components/tables/PartitionPolicyDialog.vue:55-73,215-220,553-560.
- When getPartitionPolicy fails (500, network, 422 on an older server), `stored.isPending` turns false and the second watch sets
  `loaded = true` with `formOf(null)` (the defaults). QueryBoundary then hides the form and shows the error, but the footer button
  is outside the boundary and canSave is true (loaded, valid, no retention so no type-to-confirm).
- Scenario (reproduced: form hidden, Save enabled): a policy with retention P90D and columnar conversion exists, the read fails
  once, the administrator clicks the visible "Save policy" and the stored policy is replaced by P1D, no retention, no conversion.
- Fix: set `loaded` only when stored.isSuccess; add `!stored.error.value` to canSave (and hide the button while the form is
  hidden); show the footer button only when the form is shown.

### P12W3 (medium) "Run now" is not held when the plan failed, and is a primary button although it deletes
- src/components/tables/RunPolicyDialog.vue:55 (`:confirm-disabled="plan.isPending.value"`), no `danger`.
- If the plan request fails (reproduced with a 500) isPending is false, the error is shown, and "Run now" is enabled: a run that may
  drop partitions starts without any preview. The button is btn-primary even when the plan lists drops. A plan read when the
  dialog opened is never marked old (no asOf shown, refetch on focus replaces it silently, the run uses the server time at the start).
- Mutant M6 (confirm never held) survives all 197 tests.
- Fix: `confirm-disabled = plan.isPending || plan.isError || plan.isFetching`; use danger when the plan has a drop; show the plan
  asOf; re-read the plan when it is older than a minute. Add a test for the failed plan.

### P12W4 (medium) Re-enabling or editing a policy that already has a drop retention deletes data with no preview or typed confirmation
- src/lib/analytics.ts:322-334 (lowersRetention), src/components/tables/PartitionPolicyDialog.vue:135-137.
- The check looks only at retention and its action. Scenario (reproduced in the library): stored {retention P30D, drop, enabled
  false}; the administrator ticks "The policy is on" and saves: lowersRetention is false, no preview, no name typed, and at the next
  tick the scheduler drops everything older than 30 days. The same holds for a change of partitionEvery or of the time zone that moves
  the cutoff, and for a stored value that the form cannot read (P12W5).
- Fix: treat the save as removing data whenever the next request is enabled, has a retention with action drop, and differs from
  the stored policy in any field other than a longer retention (or the stored policy was disabled); require preview and name then.

### P12W5 (medium) The form silently rewrites intervals and durations it cannot show
- src/lib/analytics.ts:254-296 (formOf), src/components/tables/PartitionPolicyDialog.vue:238-277.
- The contract allows partitionEvery P2M, P3M, P4M, P6M and P<n>Y. formOf marks them "custom", but the custom editor offers only hours
  and days and keeps the defaults (6 hours). Reproduced: P2M, P3M, P6M and P2Y all become PT6H in requestOf. Opening the dialog of a
  quarterly policy to change the retention and saving converts it to PT6H without a word. The same happens to a multi-unit
  duration: stored retention P1DT12H is shown as 90 days and sent as P90D (createAhead and runEvery likewise). The code comment
  says this is intended; nothing tells the user.
- Fix: offer months (1, 2, 3, 4, 6) and years in the custom editor; for a value the form cannot hold, keep the stored text and send
  it back unchanged, show it read-only with a note, or refuse to save until the user picks a value.

### P12W6 (medium) Health line shows a raw ISO text and loses the warning for real server values
- src/lib/analytics.ts:156-187,410-418; src/components/tables/PartitionPolicyPanel.vue (policy-ahead-text).
- The server reads partitionsAhead as an interval in iso_8601 style (`max(range_to) - now()`), for example P6DT23H59M58.123456S.
  splitIso accepts neither several units nor fractional seconds, so durationText returns the raw text (reproduced:
  "P6DT23H59M58.123456S ahead.") and healthText skips the "below the N the policy keeps" explanation. The mock returns single-unit
  values (isoFromMs, mocks/analytics.ts:119), so the tests never meet this.
- Fix: parse the full ISO form for display (years to seconds with a fraction) and print "6 days 23 hours"; compare with
  createAhead by seconds. Make the mock return the multi-unit form.

### P12W7 (medium) The mock claims blockers the server does not give before the start
- src/mocks/analytics.ts:370-382,478-486 (blockersFor: index and trigger blockers of code `other` for a plain table).
- The real preview and the real 409 for a plain table (storage_task.go tableConversionReasons) know only ledger_table,
  distributed_table, publication and foreign_keys (plus partitioned_table and partition). An index or trigger is refused later, in the
  convert step of the task (storageSQLError), so the task fails with the code conversion_refused and no typed blockers. Tests such as
  "a refusal of the server is listed reason by reason and nothing starts" pass on this fiction. The console does show a failed task, but
  the design promise "refusals appear before submit" holds only for what the server preflights.
- Also: a columnar table asked for columnar with new options is "same format" in the console (SetStorageDialog.vue:73,
  "Choose the other one") while the server accepts it (sameWithOptions: options for new stripes), so a legal change is blocked.
- Fix: align the mock (no index or trigger blockers on the plain-table path; fail the task at the convert step with
  conversion_refused), say in the dialog that indexes and triggers are checked when the task runs, and let same-format plus options
  through.

### P12W8 (low) Inline field errors of the storage and aggregates dialogs do not match the server JSON pointers
- src/components/tables/SetStorageDialog.vue:243,282,296,328,343; src/components/ApproximateAggregatesDialog.vue:89,108;
  src/mocks/analytics.ts:260-292,420-440 (fields without a leading slash).
- The server answers `/compressionLevel`, `/stripeRowCount`, `/sortBy`, `/errorRate`, `/countDistinct`; these dialogs read
  `fieldErrors.compressionLevel` etc. and the mock sends the names without the slash, so tests pass while the real error appears only in
  the general notice list, not next to the field. The policy dialog does it right (pointerField).
- Fix: key by pointerField in these dialogs; make the mock send pointers.

### P12W9 (low) Test gaps (mutants that survive)
- M6 (Run now never held): no test of a failed or slow plan.
- M8 (the default body of runPartitionPolicy gets an asOf): the rule "the panel never sends asOf" has no test that reads the body of
  the run request; the test file records bodies but not for POST .../runs.
- M17 (detach actions not summed into the removed bytes): the detach path of the preview has no assertion on the text.
- Fix: add the three tests; the asOf test should assert that the body of the run request is `{}`.

### P12W10 (low) Smaller points
- The policy dialog has five fields labelled just "Unit" (PartitionPolicyDialog.vue:264,299,329,374,460): a screen reader hears the same
  name five times. Give each a full label ("Create ahead unit").
- The preview result (policy-preview-summary) is not in a live region; only the stale and delete notices announce themselves.
- Remove policy, Run now and the storage Convert use a primary button; only the row-level Drop is red. Disabling approximate
  aggregates ("Turn off") has no typed confirmation (it is reversible and the consequence text is shown).
- usePartitionPolicy (api/queries.ts) turns every 404 into "no policy", including the server 404 "does not exist on the instance of the
  manager leader" for a table of another shard group; the panel then offers "Set a policy", which fails with the same 404.
- The mock holds a table only for state `running` (holdsTable); the server counts pending, running and paused tasks (State.Active).
- Mock 404 texts differ from the server ones ("The table has no time-partitioning policy." versus "Table s.t has no partitioning
  policy."); the console does not read them, so no harm.
- The partition format filter treats an unknown accessMethod (null) as Row (TableDetailPage.vue, shownPartitions).
- Server field errors in the policy dialog stay after the user edits the field, until the next request.

## Checked and found correct
- Bodies and queries: setTableStorage and setPartitionStorage never send dryRun; the preview sends dryRun=true as a query with the
  same body; heap sends only accessMethod (the server refuses options with heap); sortBy only for a partition and columnar; the error
  rate is sent as a fraction (percent/100, rounded); off sends only enabled false; run sends no body members; plan sends {policy} or {}.
- Policy request: ISO durations built by isoOf; presets P1D, P7D (the week is P7D, never P1W), P1M, P1Y, PT1H and custom hours that
  divide a day or days; retention and columnarAfter null when off; compression and level only with columnar; time zone trimmed, UTC
  default; the field pointers of validation_failed map to the right fields.
- asOf: the panel never sends one (code reading; M8 shows only the missing test). The mock bound of 5 minutes equals maintainSkew;
  plan has no bound on both sides.
- Preview-before-save for a retention cut: a new retention, a shorter retention, or detach to drop needs a preview of exactly the
  current form (JSON equality; a stale preview is marked, greyed and blocks Save) and the typed table name; killed by M1 to M5.
- Storage dialog: a preview of the exact body holds the Convert button while it loads, on blockers and on an error other than 422;
  the query key includes the body, so a stale preview cannot confirm; typed blockers (TableBlockerList) are shown with label and object
  through SafeName; a refusal after the start shows Problem.blockers, not just text.
- Mock versus server: operation_in_progress for put and storage during a run, conversion_refused as 409 with blockers, the asOf bound,
  idempotent put and configure, extension_missing and extension_unavailable codes.
- Role gates: every mutating button is a GateButton on the generated operation table; the server stays the authority (a 403 is
  shown through ProblemNotice). The alert subject uses typed tableSchema and tableName and links only with the right role and
  addressable names.
- Names go through SafeName; static inline widths follow the existing practice of the code base (the CSP browser test passes).
- The workbench plan summary is derived from text and never used for decisions; non-EXPLAIN results stay a table.
- Accessibility: axe suites pass in light and dark at 1366 and 320 px; dialogs trap focus; tables have captions and scoped headers.

## Mutants run (vitest: analytics-ui, analytics-lib, analytics-mock, tables-0-14-mock; 197 tests each)
| id | change | result |
|---|---|---|
| M1 | Save ignores preview freshness | killed |
| M2 | Save ignores typed name | killed |
| M3 | stale preview counts as fresh | killed |
| M4 | detach to drop not a removal | killed |
| M5 | new retention not a removal | killed |
| M6 | Run now never held | SURVIVED |
| M7 | storage preview never holds Convert | killed |
| M8 | Run now sends asOf | SURVIVED |
| M9 | table preview without dryRun | killed |
| M10 | error rate sent as percent | killed |
| M11 | retentionAction not sent | killed |
| M12 | drop warning hidden | killed |
| M13 | mock asOf bound removed | killed |
| M14 | mock put allowed during a run | killed |
| M15 | turning off sends a mode | killed |
| M16 | custom interval unit ignored | killed |
| M17 | detach not summed in preview | SURVIVED |
| M18 | Convert allowed with client blockers | killed |
| M19 | missing ratio shown as 0 | killed |
| M20 | server blockers ignored | killed |

All mutants were restored (the mutant worktree was clean afterwards). The reproductions of P12W1 to P12W6 were run as a temporary
test file, now removed.


## Fixes

All findings are fixed on branch p12-web (commit "fixes of the independent review of the console of phase 12", after the merges of
the settled contract and of the owners of tables). Each fix has a test; the mutants of the review are caught (M6, M8 and M17 now
by tests of their own) and so are the new ones N1 to N10 (listed in the work log).

| Finding | Fix |
|---|---|
| P12W1 | The preview and Run now show the partitions that would be dropped, by name and range (`PlanRemovals`). The size appears only when the server sent a size for every dropped partition; otherwise the text says "The size of the data is not known". Never 0 B. The mock's plan carries no sizes (a run fills them in); tests for both. |
| P12W2 | The form is loaded only when the stored policy was read (`isSuccess`); the Save button is not offered while the read failed; the edit button of the panel is hidden over a failed read. Test: a 500 on the read leaves no form, no Save, and the stored policy unchanged. |
| P12W3 | Run now is held while the plan is read, when it failed, and while it is read again (it is read again after a minute; the plan time is shown). The button is red when the plan drops. Tests: failed plan (M6), red and not red, the body of the run request is `{}` (M8). |
| P12W4 | `lowersRetention` also fires for a policy turned on, and for a changed interval or time zone, when the retention drops. Preview and the typed name are then needed. Library and dialog tests. |
| P12W5 | `formOf` keeps what the form cannot show (months, years, several units) in `keep` and `requestOf` sends it back unchanged; the dialog says so and offers "Change it". Tests for every kept field and for a save that changes only the retention. |
| P12W6 | Full ISO 8601 parsing (`parseIsoFull`): several units and fractional seconds are shown as "6 days 23 hours", compared by seconds, and the "below the N the policy keeps" explanation is kept. The mock sends `P2DT3H59M58.123456S`. |
| P12W7 | The mock gives no index or trigger blocker for a plain table or a partition; the task fails at the step "Convert the storage" with `conversion_refused` and the reasons in its message. The dialog says indexes and triggers are found when the task runs. A table that is columnar already can be asked for columnar again (options for new stripes); the preview says so. |
| P12W8 | The storage and aggregates dialogs read field errors by JSON pointer (`byPointer`); the mock sends pointers. Test with `/compressionLevel` from the server. |
| P12W9 | Tests for M6, M8 and M17 (the detach path of the preview). |
| P12W10 | Unique labels for the five unit fields; the preview summary is a live region; Remove policy and Run now (when it drops) are red; a 404 that says the table is on another instance is an error, not "no policy" (no "Set a policy" is offered); pending and paused tasks hold a table in the mock; an unknown access method is not counted as Row in the partitions filter. Not done: the server's 404 text for "no policy" is not read (the console shows its own words); server field errors in the policy dialog still stay until the next request. |
