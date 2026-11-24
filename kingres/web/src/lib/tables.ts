// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The words of the table lifecycle (contract 0.14.0, increment 9.7): the modes of a copy, what happens to the views that
// use a table, the lines of the report of the sharding extension (blockers, warnings, notes), who waits during a change,
// the partitions, and the tasks of a table. The codes of the extension are shown in words when they are known; a code that
// a newer server sends is shown as it was sent (through SafeName in the component), never guessed.
import type {
  DependentViews,
  Task,
  TableBlocker,
  TableChangeMode,
  TableChangePreview,
  TableOperation,
} from '@/api/types'

export const TABLE_TASK_KINDS = [
  'table.distribute',
  'table.undistribute',
  'table.alter',
  'table.partition',
  // Phase 12 (contract 0.18.0): the storage format of the table, and a run of its time-partitioning policy.
  'table.setStorage',
  'partition.maintain',
] as const

/** The largest partition name: leaf names append `_bNNNN` to it (contract `CreateTablePartitionRequest.name`). */
export const PARTITION_NAME_MAX_BYTES = 57
/** The bounds of a bucket count (contract: 1 to 4096). */
export const BUCKET_COUNT_MIN = 1
export const BUCKET_COUNT_MAX = 4096
/** `auto` copies online above this many estimated rows (contract `TableChangeMode`). */
export const ONLINE_ROWS_THRESHOLD = 100_000

export const MODE_LABEL: Record<TableChangeMode, string> = {
  auto: 'Automatic',
  online: 'Online (writers go on)',
  blocking: 'Blocking (writers wait for the whole copy)',
}

export const MODE_HINT: Record<TableChangeMode, string> = {
  auto: 'Online when the table has a usable key and more than 100000 estimated rows, otherwise blocking.',
  online:
    'Writers go on while the rows are copied and wait only for the switch. Needs a primary key or a unique index on columns that are NOT NULL.',
  blocking: 'Writers wait for the whole copy. Reads go on.',
}

export const DEPENDENT_VIEWS_LABEL: Record<DependentViews, string> = {
  refuse: 'Refuse while a view uses the table',
  recreate: 'Make the views again on the new table',
}

export const DEPENDENT_VIEWS_HINT: Record<DependentViews, string> = {
  refuse: 'A plain view that uses the table stops the change, and the table is not eligible.',
  recreate:
    'The views, and the views on them, are dropped in the switch and made again with their owner, options, privileges and comments.',
}

/** The lines of the report of the extension that the panel knows (the codes of `TableBlocker.code`). */
const BLOCKER_LABEL: Record<string, string> = {
  partitioned_table: 'A partitioned table',
  dependent_object: 'A view or another object depends on the table',
  dependent_view: 'A view uses the table',
  materialized_view: 'A materialized view uses the table',
  trigger: 'A trigger on the table',
  foreign_key_linked: 'A foreign key links the table to another',
  foreign_key: 'A foreign key links the table to another',
  no_unique_key: 'No primary or unique key',
  not_in_unique_key: 'The column is not in every unique key',
  group_busy: 'The colocation group is busy',
  not_distributed: 'The table is not distributed',
  not_plain: 'The table is already distributed',
  ledger_table: 'A ledger table',
  user_partitioned: 'A user-partitioned table',
  // Phase 12 (contract 0.18.0, `TableStoragePreview` and `conversion_refused`).
  partition: 'A partition that cannot be converted',
  distributed_table: 'A distributed table',
  publication: 'A publication',
  foreign_keys: 'Foreign keys',
  other: 'Refused by the extension',
  owner_not_trusted: 'The owner of the table is not trusted',
  free_space: 'Not enough free space',
}

export function tableBlockerLabel(b: Pick<TableBlocker, 'code'>): string {
  return BLOCKER_LABEL[b.code] ?? 'Reported by the sharding extension'
}

export const WRITERS_BLOCKED_TEXT: Record<
  TableChangePreview['estimate']['writersBlocked'],
  string
> = {
  none: 'Nobody waits: only the catalog changes.',
  switchOnly: 'Writers wait only for the switch at the end.',
  wholeCopy: 'Writers wait for the whole copy; reads go on.',
}

export const AFFECTED_ROLE_LABEL: Record<string, string> = {
  source: 'The table',
  colocated: 'Changes with it (same colocation group)',
  linked: 'Linked by a foreign key',
}

export function affectedRoleLabel(role: string): string {
  return AFFECTED_ROLE_LABEL[role] ?? role
}

/** How many bytes a name takes as UTF-8, which is what the 57 bytes of a partition name count. */
export function utf8Length(text: string): number {
  return new TextEncoder().encode(text).length
}

/** What is wrong with the name of a new partition, or null. */
export function partitionNameError(name: string): string | null {
  if (name.trim() === '') return 'Give the partition a name.'
  if (utf8Length(name) > PARTITION_NAME_MAX_BYTES)
    return `Use at most ${PARTITION_NAME_MAX_BYTES} bytes: the name of each leaf adds a suffix.`
  return null
}

/** What is wrong with the bound of a partition (the text that follows FOR VALUES, or DEFAULT), or null. */
export function partitionBoundError(bound: string): string | null {
  if (bound.trim() === '')
    return "Give the bound, for example FROM ('2026-01-01') TO ('2026-02-01'), or DEFAULT."
  return null
}

/** What is wrong with a bucket count typed into a form (the empty text is allowed: the database setting is used), or null. */
export function bucketCountError(text: string): string | null {
  const t = text.trim()
  if (t === '') return null
  if (!/^\d+$/.test(t)) return 'Use a whole number.'
  const n = Number(t)
  if (n < BUCKET_COUNT_MIN || n > BUCKET_COUNT_MAX)
    return `Use a number from ${BUCKET_COUNT_MIN} to ${BUCKET_COUNT_MAX}.`
  return null
}

/** The tasks of one table, newest first (`subject.id` of a table is `database/schema.table`). */
export function tableSubjectId(database: string, schema: string, table: string): string {
  return `${database}/${schema}.${table}`
}

/**
 * The tasks of one table. "partitions" are the names of its partitions: the task table.setStorage of one partition has the
 * subject database/schema.partition (contract 0.18.0), and it holds the table too.
 */
export function tasksOfTable(
  tasks: Task[],
  database: string,
  schema: string,
  table: string,
  partitions: readonly string[] = [],
): Task[] {
  const id = tableSubjectId(database, schema, table)
  const parts = new Set(partitions.map((p) => tableSubjectId(database, schema, p)))
  return tasks.filter(
    (t) =>
      t.subject?.type === 'table' &&
      (t.subject?.id === id || (t.kind === 'table.setStorage' && parts.has(t.subject?.id ?? ''))) &&
      (TABLE_TASK_KINDS as readonly string[]).includes(t.kind),
  )
}

/**
 * What a dialog says after a task was cancelled. The control plane records the task as cancelled first and undoes the change after
 * that, and it can fail to reach a server, so this never says that the table is as it was; the detail of the task, shown with it,
 * says what the control plane reports (the undo done, or running, or a server that is cleaned up when it returns).
 */
export const CANCELLED_TEXT =
  'Cancelled. The control plane undoes what the change had begun; what it reports about that is shown here and may name a server that is cleaned up when it returns.'

/** How many of the newest tasks the page reads to find the changes of its table (the contract cannot filter by subject). */
export const TABLE_TASKS_LIMIT = 200

/**
 * The task that holds the table now, if any: a change of it waits to start, runs or is paused. This is an early hint only: a task
 * older than the newest TABLE_TASKS_LIMIT is not seen, and the server stays the authority (409 `operation_in_progress`, which the
 * dialogs show in words).
 */
export function runningTaskOfTable(
  tasks: Task[],
  database: string,
  schema: string,
  table: string,
  partitions: readonly string[] = [],
): Task | null {
  return (
    tasksOfTable(tasks, database, schema, table, partitions).find(
      (t) => t.state === 'pending' || t.state === 'running' || t.state === 'paused',
    ) ?? null
  )
}

/** The blockers that make a change refused (they disable the start button); the warnings and notes never do. */
export function previewBlocked(preview: TableChangePreview | undefined): boolean {
  return (preview?.blockers.length ?? 0) > 0
}

// --- the operation indicator of a table (contract 0.32.0, `Table.operation`) -------------------------------------------

/** What an operation is called in a sentence ("A distribution of this table is running"). */
const OPERATION_NOUN: Record<string, string> = {
  distribute: 'distribution',
  undistribute: 'undistribution',
  alter: 'change of the distribution',
  partition: 'change of the partitions',
  convertStorage: 'conversion of the storage format',
  maintain: 'run of the time-partitioning policy',
  export: 'export',
  import: 'import',
}

/** The short word of the badge. A kind that a newer server sends is shown as it was sent (the component puts it through SafeName). */
export const OPERATION_SHORT: Record<string, string> = {
  distribute: 'Distributing',
  undistribute: 'Undistributing',
  alter: 'Changing distribution',
  partition: 'Changing partitions',
  convertStorage: 'Converting storage',
  maintain: 'Maintaining partitions',
  export: 'Exporting',
  import: 'Importing',
}

export function operationShort(op: TableOperation): string {
  return OPERATION_SHORT[op.kind] ?? op.kind
}

/** The sentence that says why the changes of a table are off: the operation that runs, and the task that has it (it starts as the sentence of the tasks of a table: "A change of this table is running"). */
export function operationReason(op: TableOperation | null | undefined): string {
  if (!op) return ''
  const noun = OPERATION_NOUN[op.kind] ?? `operation (${op.kind})`
  const who =
    op.taskId !== null
      ? ` (task ${op.taskId})`
      : op.source === 'extension'
        ? ' (started from SQL)'
        : ''
  const subject =
    op.role === 'related'
      ? 'A change of another table that takes this table along'
      : 'A change of this table'
  return `${subject} is running${who}: ${noun}.`
}

/** The progress text of the badge: the percent when it is known, the state otherwise. */
export function operationProgress(op: TableOperation): string {
  return op.percent !== null && op.state !== 'pending' ? `${Math.round(op.percent)}%` : op.state
}
