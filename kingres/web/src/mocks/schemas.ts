// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The five operations that contract 0.15.0 added to the tag `Schemas` (listSchemas, getSchema, distributeSchema,
// undistributeSchema, moveSchema) for the mock API, and the schema-aware parts of the database operations (the sharding
// mode, the colocation groups of kind `schema`, the schemas per server). They follow the control plane: a distribute that
// meets blockers is a 422 `schema_not_distributable` with the blockers in `errors`, a move of tables without a key is a
// 422 `unprocessable` that names them unless `setReplicaIdentityFull` is sent, a move task is done at the phase `cleaning`,
// and a schema that is moving, or pinned by a ledger table, is refused with its own code.
import type { components } from '@/api/schema'
import type { OperationId } from '@/api/operations.generated'
import { accepted, ok, paginate, problem, validationFailed } from './http'
import type { Ctx, Handler } from './http'
import { createTask, dataGroups } from './state'
import type { DbRec, MockState, SchemaRec, TaskRec } from './state'

type S = components['schemas']

const body = <T>(ctx: Ctx): T => (ctx.body ?? {}) as T

function db(s: MockState, name: string): DbRec {
  const rec = s.databases.find((d) => d.info.name === name)
  if (!rec) throw problem('not_found', `Database "${name}" does not exist.`)
  return rec
}

/** The sharding mode of a database; a database that does not say has `row` (every database before 0.15.0). */
export const modeOf = (rec: DbRec): 'row' | 'schema' | 'both' => rec.info.sharding.mode ?? 'row'

/**
 * The schemas of a database. A database that has none recorded lists the schemas of its tables as local schemas (every
 * schema of a database without distributed schemas has `distribution: null`).
 */
export function schemasOf(rec: DbRec): SchemaRec[] {
  if (rec.schemas) return rec.schemas
  const names = [...new Set(rec.tables.map((t) => t.schema))].sort()
  rec.schemas = names.map((name) => {
    const own = rec.tables.filter((t) => t.schema === name)
    const sizes = own.map((t) => t.sizeBytes)
    const rows = own.map((t) => t.estimatedRows)
    return {
      name,
      owner: rec.info.general.owner ?? 'app_rw',
      group: null,
      colocationId: 0,
      state: 'active' as const,
      movingTo: null,
      taskId: null,
      tableCount: own.length,
      sizeBytes: sizes.some((n) => n === null)
        ? null
        : sizes.reduce<number>((a, n) => a + (n ?? 0), 0),
      estimatedRows: rows.some((n) => n === null)
        ? null
        : rows.reduce<number>((a, n) => a + (n ?? 0), 0),
      blockers: name === 'public' ? [PUBLIC_BLOCKER] : [],
      pinned: null,
      keyless: [],
    }
  })
  return rec.schemas
}

const PUBLIC_BLOCKER: S['SchemaBlocker'] = {
  objectKind: 'schema',
  objectName: 'public',
  reason: 'the schema public cannot be distributed',
}

const serverOfGroup = (s: MockState, g: number): string | null =>
  s.groups.find((x) => x.id === g)?.primaryServerId ?? null

export function schemaSummary(s: MockState, sr: SchemaRec): S['SchemaSummary'] {
  return {
    name: sr.name,
    owner: sr.owner,
    distribution:
      sr.group === null
        ? null
        : {
            state: sr.state,
            colocationId: sr.colocationId,
            shardGroupId: sr.group,
            serverId: serverOfGroup(s, sr.group),
            movingToShardGroupId: sr.movingTo,
          },
    tableCount: sr.tableCount,
    sizeBytes: sr.sizeBytes,
    estimatedRows: sr.estimatedRows,
  }
}

function schemaDetail(s: MockState, sr: SchemaRec): S['SchemaDetail'] {
  return {
    ...schemaSummary(s, sr),
    // A schema that is distributed has no blockers: the inventory is of what is still to be done.
    distributionBlockers: sr.group === null ? structuredClone(sr.blockers) : [],
    pinned: sr.pinned === null ? null : { reason: sr.pinned },
    moveInProgress: sr.taskId,
  }
}

function schema(s: MockState, c: Ctx): { rec: DbRec; sr: SchemaRec } {
  const rec = db(s, c.params.databaseName ?? '')
  const sr = schemasOf(rec).find((x) => x.name === c.params.schemaName)
  if (!sr) throw problem('not_found', `Schema "${c.params.schemaName}" does not exist.`)
  return { rec, sr }
}

/** The distributed schemas of a database. */
export const distributedSchemas = (rec: DbRec): SchemaRec[] =>
  schemasOf(rec).filter((x) => x.group !== null)

/** The colocation groups of kind `schema`: one bucket for every distributed schema. */
export function schemaColocations(rec: DbRec): S['ColocationGroup'][] {
  return distributedSchemas(rec).map((x) => ({
    colocationId: x.colocationId,
    kind: 'schema' as const,
    schemaName: x.name,
    bucketCount: 1,
    distributionColumnType: null,
    tableCount: x.tableCount,
    sizeBytes: x.sizeBytes,
  }))
}

/** Distributed schemas, and their size, that have their primary copy on a server (`ServerBucketCount.primarySchemas`). */
export function schemasOnServer(
  s: MockState,
  rec: DbRec,
  serverId: string,
): { count: number; bytes: number } {
  let count = 0
  let bytes = 0
  for (const x of distributedSchemas(rec)) {
    if (x.group !== null && serverOfGroup(s, x.group) === serverId) {
      count += 1
      bytes += x.sizeBytes ?? 0
    }
  }
  return { count, bytes }
}

/** The moves of schemas that run now, as `BucketMove` items of the rebalance status (kind `schema`, bucket 0). */
export function schemaMovesInFlight(s: MockState): S['BucketMove'][] {
  const out: S['BucketMove'][] = []
  for (const rec of s.databases) {
    for (const x of distributedSchemas(rec)) {
      if (x.state !== 'moving' || x.movingTo === null || x.group === null) continue
      const task = s.tasks.find((t) => t.task.id === x.taskId)
      const percent = task?.task.progress.percent ?? 0
      const phases = ['prepare', 'copy', 'catchUp', 'switch', 'cleanup'] as const
      const phase = phases[Math.min(4, Math.floor(percent / 20))] as (typeof phases)[number]
      out.push({
        kind: 'schema',
        schemaName: x.name,
        databaseName: rec.info.name,
        colocationId: x.colocationId,
        bucket: 0,
        fromShardGroupId: x.group,
        toShardGroupId: x.movingTo,
        fromServerId: serverOfGroup(s, x.group) ?? undefined,
        toServerId: serverOfGroup(s, x.movingTo) ?? undefined,
        phase,
        percent: [5, 30, 85, 95, 100][phases.indexOf(phase)] as number,
        sizeBytes: x.sizeBytes,
      })
    }
  }
  return out
}

function requireSharding(rec: DbRec): void {
  if (!rec.info.sharding.enabled)
    throw problem('conflict', 'Sharding is not enabled for this database.')
}

function requireDataGroup(s: MockState, id: number, field: string): void {
  if (!dataGroups(s).some((g) => g.id === id))
    throw validationFailed([
      {
        field,
        message: `Shard group ${id} does not exist or holds no shards.`,
        code: 'unknown_value',
      },
    ])
}

function nextColocationId(rec: DbRec): number {
  return (
    Math.max(
      0,
      ...rec.tables.map((t) => t.distribution?.colocationId ?? 0),
      ...rec.colocations.map((g) => g.colocationId),
      ...schemasOf(rec).map((x) => x.colocationId),
    ) + 1
  )
}

/** The group the placement rule chooses: the data group with the fewest distributed schemas. */
function placement(s: MockState, rec: DbRec): number {
  const load = (id: number) => distributedSchemas(rec).filter((x) => x.group === id).length
  return [...dataGroups(s)].sort((a, b) => load(a.id) - load(b.id) || a.id - b.id)[0]?.id ?? 1
}

const SCHEMA_SUBJECT = (rec: DbRec, sr: SchemaRec) => ({
  type: 'schema' as const,
  id: `${rec.info.name}/${sr.name}`,
})

function distribute(c: Ctx) {
  const s = c.s
  const { rec, sr } = schema(s, c)
  const b = body<S['DistributeSchemaRequest']>(c)
  requireSharding(rec)
  if (modeOf(rec) === 'row')
    throw problem(
      'conflict',
      'The sharding mode of this database is row. Change it to schema or both first.',
    )
  if (sr.group !== null) throw problem('conflict', `Schema "${sr.name}" is already distributed.`)
  if (sr.blockers.length > 0) {
    const p = problem(
      'schema_not_distributable',
      `Schema "${sr.name}" cannot be distributed: ${sr.blockers.length} object(s) are not supported.`,
      {
        errors: sr.blockers.map((x) => ({
          field: x.objectName,
          message: `${x.objectKind} ${x.objectName}: ${x.reason}`,
          code: x.reason,
        })),
      },
    )
    throw p
  }
  if (b.shardGroupId !== undefined && b.shardGroupId !== null)
    requireDataGroup(s, b.shardGroupId, '/shardGroupId')
  // A schema that has rows stays where its rows are (the group of the map group's server).
  const rowsGroup = dataGroups(s).find(
    (g) => g.primaryServerId === serverOfGroup(s, 0) || g.id === 1,
  )?.id as number
  const hasRows = sr.tableCount > 0 && (sr.sizeBytes ?? 1) > 0
  const wanted = b.shardGroupId ?? null
  const target = hasRows ? rowsGroup : (wanted ?? placement(s, rec))
  const refused =
    hasRows && wanted !== null && wanted !== rowsGroup
      ? `Schema "${sr.name}" has rows on shard group ${rowsGroup}, not on ${wanted}. Distribute it there and move it afterwards.`
      : undefined
  return accepted(
    createTask(s, {
      kind: 'schema.distribute',
      subject: SCHEMA_SUBJECT(rec, sr),
      steps: [
        'Check the schema',
        'Set REPLICA IDENTITY FULL',
        'Make the schema distributed',
        'Verify on every server',
      ],
      skippedStep: b.setReplicaIdentityFull === true ? undefined : 'Set REPLICA IDENTITY FULL',
      failWith: refused,
      apply: () => {
        sr.group = target
        sr.colocationId = nextColocationId(rec)
        sr.state = 'active'
        if (b.setReplicaIdentityFull === true) sr.keyless = []
      },
    }),
  )
}

function undistribute(c: Ctx) {
  const s = c.s
  const { rec, sr } = schema(s, c)
  if (sr.group === null) throw problem('conflict', `Schema "${sr.name}" is not distributed.`)
  if (sr.state === 'moving')
    throw problem(
      'schema_move_in_progress',
      `Schema "${sr.name}" is moving to group ${sr.movingTo}.`,
    )
  if (sr.pinned !== null) throw problem('schema_pinned', sr.pinned)
  const raw = c.url.searchParams.get('shardGroupId')
  const first = raw === null ? null : Number(raw)
  if (first !== null) {
    if (!Number.isInteger(first))
      throw validationFailed([
        { field: 'shardGroupId', message: 'The shard group must be a whole number.', code: 'type' },
      ])
    requireDataGroup(s, first, 'shardGroupId')
  }
  const moves = first !== null && first !== sr.group
  return accepted(
    createTask(s, {
      kind: 'schema.undistribute',
      subject: SCHEMA_SUBJECT(rec, sr),
      steps: ['Check the schema', 'Move first', 'Make the schema local', 'Verify on every server'],
      skippedStep: moves ? undefined : 'Move first',
      apply: () => {
        sr.group = null
        sr.colocationId = 0
        sr.state = 'active'
      },
    }),
  )
}

/** Starts the move of a distributed schema and returns its task. Used by the handler and by tests that need a moving schema. */
export function beginSchemaMove(
  s: MockState,
  rec: DbRec,
  sr: SchemaRec,
  target: number,
  setIdentity: boolean,
) {
  let task: S['Task'] | null = null
  const id = () => (task as S['Task']).id
  sr.state = 'moving'
  sr.movingTo = target
  const settle = () => {
    sr.state = 'active'
    sr.movingTo = null
    sr.taskId = null
  }
  task = createTask(s, {
    kind: 'schema.move',
    subject: SCHEMA_SUBJECT(rec, sr),
    steps: [
      'Check the move',
      'Set REPLICA IDENTITY FULL',
      'Copy the data',
      'Switch',
      'Clean up the old copy',
    ],
    skippedStep: setIdentity ? undefined : 'Set REPLICA IDENTITY FULL',
    cancellable: true,
    durationMs: 4000,
    failWith: sr.failsToMove
      ? `The copy of schema "${sr.name}" to shard group ${target} failed: the target ran out of disk space.`
      : undefined,
    onProgress: (fraction) => {
      const rec2 = s.tasks.find((t) => t.task.id === id())
      const step = rec2?.task.progress.steps[2]
      const total = sr.sizeBytes ?? 8 * 1024 * 1024
      if (step) {
        step.itemsTotal = total
        step.itemsDone = Math.min(total, Math.round(total * Math.min(1, fraction / 0.6)))
      }
    },
    onFail: settle,
    apply: () => {
      sr.group = target
      sr.state = 'active'
      sr.movingTo = null
      sr.taskId = null
      if (setIdentity) sr.keyless = []
    },
  })
  sr.taskId = task.id
  return task
}

function move(c: Ctx) {
  const s = c.s
  const { rec, sr } = schema(s, c)
  const b = body<S['MoveSchemaRequest']>(c)
  if (!Number.isInteger(b.targetShardGroupId) || b.targetShardGroupId < 0)
    throw validationFailed([
      {
        field: '/targetShardGroupId',
        message: 'The target shard group is required.',
        code: 'required',
      },
    ])
  if (s.rebalance.state === 'running' || s.rebalance.state === 'stopping')
    throw problem('rebalance_in_progress', 'A rebalance is running.')
  if (sr.group === null) throw problem('conflict', `Schema "${sr.name}" is not distributed.`)
  if (sr.state === 'moving')
    throw problem(
      'schema_move_in_progress',
      `Schema "${sr.name}" is moving to group ${sr.movingTo}.`,
    )
  if (sr.pinned !== null) throw problem('schema_pinned', sr.pinned)
  if (sr.group === b.targetShardGroupId)
    throw problem('conflict', `Schema "${sr.name}" is already on shard group ${sr.group}.`)
  requireDataGroup(s, b.targetShardGroupId, '/targetShardGroupId')
  if (sr.keyless.length > 0 && b.setReplicaIdentityFull !== true)
    throw problem(
      'unprocessable',
      `Tables without a primary key and without REPLICA IDENTITY FULL cannot be moved: ${sr.keyless.join(', ')}. Add a key, or ask for setReplicaIdentityFull.`,
    )
  return accepted(
    beginSchemaMove(s, rec, sr, b.targetShardGroupId, b.setReplicaIdentityFull === true),
  )
}

/** `cancelTask` of a schema move: before the switch the schema stays on the source, after it the task cannot be cancelled. */
export function cancelSchemaMove(s: MockState, rec: TaskRec): S['Task'] {
  if (rec.task.state !== 'running' || rec.task.progress.percent >= 60)
    throw problem('task_not_cancellable', 'The move has reached the switch.')
  const [dbName = '', schemaName = ''] = (rec.task.subject?.id ?? '').split('/')
  const sr = s.databases
    .find((d) => d.info.name === dbName)
    ?.schemas?.find((x) => x.name === schemaName)
  if (sr) {
    sr.state = 'active'
    sr.movingTo = null
    sr.taskId = null
  }
  rec.task.state = 'failed'
  rec.task.cancellable = false
  rec.task.error = {
    code: 'cancelled',
    message: 'The task was canceled. The schema is still on its original group.',
    retryable: false,
  }
  rec.applied = true
  return rec.task
}

export function schemaHandlers(): Partial<Record<OperationId, Handler>> {
  return {
    listSchemas: (c) => {
      const rec = db(c.s, c.params.databaseName ?? '')
      const distributed = c.url.searchParams.get('distributed')
      const group = c.url.searchParams.get('shardGroupId')
      const state = c.url.searchParams.get('state')
      const items = [...schemasOf(rec)]
        .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0))
        .filter(
          (x) =>
            (distributed === null || (distributed === 'true') === (x.group !== null)) &&
            (group === null || x.group === Number(group)) &&
            (state === null || (x.group !== null && x.state === state)),
        )
        .map((x) => schemaSummary(c.s, x))
      return ok(paginate(items, c.url))
    },
    getSchema: (c) => ok(schemaDetail(c.s, schema(c.s, c).sr)),
    distributeSchema: distribute,
    undistributeSchema: undistribute,
    moveSchema: move,
  }
}
