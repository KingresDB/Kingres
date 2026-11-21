// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The mock of contract 0.16.0 (phases 10 and 11): the drain of a server, the split of a bucket, the schemas of a database and the
// distribution of a schema. Schemas named `failme` fail their task, `analytics` has an object that cannot be distributed,
// `ledgerdocs` is pinned by a ledger table, `nokey` has a table without a key, and one name is hostile on purpose.
import type { components } from '@/api/schema'
import type { OperationId } from '@/api/operations.generated'
import { accepted, ok, paginate, problem, validationFailed } from './http'
import type { Ctx, Handler } from './http'
import { createTask, dataGroups } from './state'
import type { DbRec, MockState } from './state'

type S = components['schemas']
const body = <T>(c: Ctx): T => (c.body ?? {}) as T

export interface MockSchema {
  name: string
  owner: string
  group: number | null
  moving: boolean
  tableCount: number
  sizeBytes: number | null
  blockers: S['SchemaBlocker'][]
  pinned: string | null
  keyless: string[]
}

const HOSTILE = '<img src=x onerror=alert(1)>'

function seed(): MockSchema[] {
  const mk = (name: string, group: number | null, extra: Partial<MockSchema> = {}): MockSchema => ({
    name,
    owner: 'app',
    group,
    moving: false,
    tableCount: 4,
    sizeBytes: group === null ? null : 48 * 1024 * 1024,
    blockers: [],
    pinned: null,
    keyless: [],
    ...extra,
  })
  return [
    mk('analytics', null, {
      blockers: [
        {
          objectKind: 'materialized view',
          objectName: 'analytics.daily',
          reason: 'materialized views are not supported in distributed schemas yet',
        },
      ],
    }),
    mk('failme', null),
    mk('legacy', null),
    mk(HOSTILE, null, { tableCount: 1 }),
    mk('ledgerdocs', 1, {
      pinned:
        'schema "ledgerdocs" holds the ledger table docs; the chain of the ledger is on that group',
    }),
    mk('nokey', 1, { keyless: ['events'] }),
    mk('tenant_1', 1),
    mk('tenant_2', 2),
  ]
}

function schemasOf(rec: DbRec & { schemas?: MockSchema[] }): MockSchema[] {
  rec.schemas ??= rec.info.sharding.enabled ? seed() : []
  return rec.schemas
}

/** The distributed schemas of a database (the update of the sharding mode refuses Rows only while there are any). */
export function distributedSchemaCount(rec: DbRec): number {
  return schemasOf(rec).filter((m) => m.group !== null).length
}

function db(s: MockState, name: string): DbRec & { schemas?: MockSchema[] } {
  const rec = s.databases.find((d) => d.info.name === name)
  if (!rec) throw problem('not_found', `Database "${name}" does not exist.`)
  return rec
}

const serverOfGroup = (s: MockState, g: number): string | null =>
  s.groups.find((x) => x.id === g)?.preferredPrimaryServerId ?? null

function summary(s: MockState, m: MockSchema): S['SchemaSummary'] {
  return {
    name: m.name,
    owner: m.owner,
    distribution:
      m.group === null
        ? null
        : {
            state: m.moving ? 'moving' : 'active',
            colocationId: 100 + (m.group ?? 0),
            shardGroupId: m.group,
            serverId: serverOfGroup(s, m.group),
            movingToShardGroupId: null,
          },
    tableCount: m.tableCount,
    sizeBytes: m.sizeBytes,
    estimatedRows: m.tableCount * 10_000,
  }
}

function schema(rec: DbRec & { schemas?: MockSchema[] }, name: string): MockSchema {
  const m = schemasOf(rec).find((x) => x.name === name)
  if (!m) throw problem('not_found', `Schema "${name}" does not exist.`)
  return m
}

/** Adds what contract 0.16.0 says about a run to the status of the mock. */
export function decorateStatus(s: MockState, st: S['RebalanceStatus']): S['RebalanceStatus'] {
  const auto = s.settings.rebalance.automatic
  return {
    ...st,
    strategy: st.options?.strategy ?? s.settings.rebalance.strategy ?? 'byBucketCount',
    threshold: st.options?.threshold ?? 0,
    trigger: s.drain ? 'drain' : 'manual',
    automatic: {
      enabled: auto?.enabled ?? false,
      nextCheckAt: auto?.enabled ? new Date(Date.now() + 40_000).toISOString() : null,
      lastEvaluation: auto?.enabled
        ? { at: new Date(Date.now() - 20_000).toISOString(), outcome: 'settling', blockedBy: [] }
        : null,
    },
  }
}

const sizes = (m: number) => m * 1024 * 1024

export function elasticityHandlers(): Partial<Record<OperationId, Handler>> {
  return {
    previewServerDrain: (c) => {
      const s = c.s
      const id = c.params.serverId ?? ''
      const srv = s.servers.find((x) => x.id === id)
      if (!srv) throw problem('not_found', `Server ${id} does not exist.`)
      const owned = dataGroups(s).filter((g) => g.preferredPrimaryServerId === id)
      const moves: S['PlannedMove'][] = []
      const others = dataGroups(s).filter((g) => g.preferredPrimaryServerId !== id)
      let i = 0
      for (const g of owned) {
        for (let b = 0; b < 6; b++) {
          const to = others[i++ % Math.max(1, others.length)]
          moves.push({
            databaseName: 'shop',
            colocationId: 1,
            bucket: g.id * 10 + b,
            fromShardGroupId: g.id,
            toShardGroupId: to?.id ?? 0,
            fromServerId: id,
            toServerId: to ? (serverOfGroup(s, to.id) ?? undefined) : undefined,
            estimatedSizeBytes: sizes(64),
            rank: 1000 - i,
            reason: 'drain',
            cost: sizes(64),
          })
        }
      }
      const blockers: S['DrainBlocker'][] = []
      if (s.servers.length <= 1)
        blockers.push({ code: 'last_server', detail: 'This is the last server.' })
      const canKeep = s.servers.length - 1 >= s.settings.defaultReplicaCount + 1
      return ok({
        serverId: id,
        canDrain: blockers.length === 0,
        strategy: 'byBucketCount',
        moveCount: moves.length,
        totalBytes: moves.length * sizes(64),
        moves,
        standbyShardGroupIds: dataGroups(s)
          .slice(0, 1)
          .map((g) => g.id),
        canKeepReplicas: canKeep,
        localData: id.endsWith('13')
          ? [{ databaseName: 'scratch', tableCount: 2, sizeBytes: sizes(12) }]
          : [],
        blockers,
        warnings: [],
      } satisfies S['DrainPreview'])
    },
    drainServer: (c) => {
      const s = c.s
      const id = c.params.serverId ?? ''
      const srv = s.servers.find((x) => x.id === id)
      if (!srv) throw problem('not_found', `Server ${id} does not exist.`)
      const b = body<S['DrainRequest']>(c)
      if (s.servers.length <= 1) throw problem('last_server', 'This is the last server.')
      if (s.settings.defaultReplicaCount + 1 > s.servers.length - 1 && !b.acceptFewerReplicas)
        throw problem(
          'replicas_cannot_be_kept',
          'Fewer servers would remain than the replica count needs.',
        )
      if (id.endsWith('13') && !b.acknowledgeLocalData)
        throw problem('unprocessable', 'The server holds ordinary tables that leave with it.')
      s.drain = id
      return accepted(
        createTask(s, {
          kind: 'rebalance',
          subject: { type: 'cluster', id: 'cluster' },
          steps: [
            'Check that the cluster can rebalance',
            'Stop new buckets from landing on the server',
            'Compute the plan',
            'Move buckets',
            'Wait until the server owns no bucket',
            'Relocate the standbys hosted on the server',
            'Write the report',
          ],
          apply: () => {
            srv.shouldHaveShards = false
            s.drain = null
          },
        }),
      )
    },
    previewBucketSplit: (c) => {
      const rec = db(c.s, c.params.databaseName ?? '')
      const colo = Number(c.params.colocationId)
      const bucket = Number(c.params.bucket)
      const b = body<S['BucketSplitRequest']>(c)
      const parts = b.parts ?? 2
      const list = rec.buckets.get(colo)
      const cur = list?.find((x) => x.bucket === bucket)
      if (!cur) throw problem('not_found', `Bucket ${bucket} does not exist.`)
      const modulus = list?.length ?? 1
      const blockers: S['DrainBlocker'][] = []
      if (cur.state !== 'active')
        blockers.push({ code: 'bucket_busy', detail: `The bucket is ${cur.state}.` })
      const size = cur.size
      return ok({
        canSplit: blockers.length === 0,
        parts,
        children: Array.from({ length: parts }, (_, i) => ({
          bucket: bucket + i * modulus,
          modulus: modulus * parts,
          remainder: bucket + i * modulus,
          estimatedSizeBytes: size === null ? null : Math.floor(size / parts),
        })),
        estimatedSizeBytes: size,
        extraDiskBytes: size,
        blockers,
      } satisfies S['BucketSplitPreview'])
    },
    splitBucket: (c) => {
      const rec = db(c.params.databaseName ? c.s : c.s, c.params.databaseName ?? '')
      const colo = Number(c.params.colocationId)
      const bucket = Number(c.params.bucket)
      const parts = body<S['BucketSplitRequest']>(c).parts ?? 2
      const list = rec.buckets.get(colo)
      const cur = list?.find((x) => x.bucket === bucket)
      if (!list || !cur) throw problem('not_found', `Bucket ${bucket} does not exist.`)
      if (cur.state !== 'active') throw problem('bucket_busy', `The bucket is ${cur.state}.`)
      const modulus = list.length
      return accepted(
        createTask(c.s, {
          kind: 'bucket.split',
          subject: { type: 'bucket', id: `${rec.info.name}/${colo}/${bucket}` },
          steps: ['Check that the split can start', 'Split the bucket', 'Check the new buckets'],
          detail: 'copying: 0 of 10000 rows',
          apply: () => {
            const size = cur.size === null ? null : Math.floor(cur.size / parts)
            cur.size = size
            for (let i = 1; i < parts; i++)
              list.push({
                bucket: bucket + i * modulus,
                group: cur.group,
                state: 'active',
                movingTo: null,
                size,
              })
            list.sort((a, z) => a.bucket - z.bucket)
            const g = rec.colocations.find((x) => x.colocationId === colo)
            if (g) g.bucketCount = list.length
          },
        }),
      )
    },
    listSchemas: (c) => {
      const rec = db(c.s, c.params.databaseName ?? '')
      let rows = schemasOf(rec)
      const q = c.url.searchParams
      if (q.get('distributed') !== null)
        rows = rows.filter((m) => (m.group !== null) === (q.get('distributed') === 'true'))
      if (q.get('shardGroupId') !== null)
        rows = rows.filter((m) => m.group === Number(q.get('shardGroupId')))
      if (q.get('state') !== null)
        rows = rows.filter(
          (m) => m.group !== null && (m.moving ? 'moving' : 'active') === q.get('state'),
        )
      return ok(
        paginate(
          rows.map((m) => summary(c.s, m)),
          c.url,
        ),
      )
    },
    getSchema: (c) => {
      const rec = db(c.s, c.params.databaseName ?? '')
      const m = schema(rec, c.params.schemaName ?? '')
      return ok({
        ...summary(c.s, m),
        distributionBlockers: m.group === null ? m.blockers : [],
        pinned: m.pinned ? { reason: m.pinned } : null,
        moveInProgress: m.moving ? 9000 : null,
        tablesWithoutReplicaIdentity: m.keyless,
      } satisfies S['SchemaDetail'])
    },
    distributeSchema: (c) => {
      const rec = db(c.s, c.params.databaseName ?? '')
      const m = schema(rec, c.params.schemaName ?? '')
      const b = body<S['DistributeSchemaRequest']>(c)
      if (m.group !== null) throw problem('conflict', `Schema "${m.name}" is already distributed.`)
      if (m.blockers.length > 0)
        throw problem('schema_not_distributable', 'The schema cannot be distributed.', {
          errors: m.blockers.map((x) => ({
            field: x.objectName,
            message: `${x.objectKind}: ${x.reason}`,
          })),
        })
      const target = b.shardGroupId ?? dataGroups(c.s)[0]?.id ?? 1
      return accepted(
        createTask(c.s, {
          kind: 'schema.distribute',
          subject: { type: 'schema', id: `${rec.info.name}/${m.name}` },
          steps: [
            'Check the schema',
            'Set REPLICA IDENTITY FULL',
            'Make the schema distributed',
            'Verify on every server',
          ],
          failWith:
            m.name === 'failme'
              ? 'distribute_schema failed: role "app" does not exist on a server'
              : undefined,
          apply: () => {
            m.group = target
            m.sizeBytes = sizes(32)
          },
        }),
      )
    },
    undistributeSchema: (c) => {
      const rec = db(c.s, c.params.databaseName ?? '')
      const m = schema(rec, c.params.schemaName ?? '')
      if (m.group === null) throw problem('conflict', `Schema "${m.name}" is not distributed.`)
      if (m.pinned) throw problem('schema_pinned', m.pinned)
      if (m.moving) throw problem('schema_move_in_progress', 'The schema is being moved.')
      return accepted(
        createTask(c.s, {
          kind: 'schema.undistribute',
          subject: { type: 'schema', id: `${rec.info.name}/${m.name}` },
          steps: [
            'Check the schema',
            'Move first',
            'Make the schema local',
            'Verify on every server',
          ],
          apply: () => {
            m.group = null
            m.sizeBytes = null
          },
        }),
      )
    },
    moveSchema: (c) => {
      const rec = db(c.s, c.params.databaseName ?? '')
      const m = schema(rec, c.params.schemaName ?? '')
      const b = body<S['MoveSchemaRequest']>(c)
      if (m.group === null) throw problem('conflict', `Schema "${m.name}" is not distributed.`)
      if (typeof b.targetShardGroupId !== 'number')
        throw validationFailed([
          { field: '/targetShardGroupId', message: 'Give the target shard group.' },
        ])
      if (m.pinned) throw problem('schema_pinned', m.pinned)
      if (m.moving) throw problem('schema_move_in_progress', 'The schema is being moved.')
      if (b.targetShardGroupId === m.group)
        throw problem('unprocessable', 'The schema is on that shard group already.')
      if (m.keyless.length > 0 && !b.setReplicaIdentityFull)
        throw problem(
          'unprocessable',
          'Tables without a primary key or replica identity cannot be moved.',
          {
            errors: m.keyless.map((t) => ({
              field: t,
              message: 'no primary key and no replica identity',
            })),
          },
        )
      m.moving = true
      return accepted(
        createTask(c.s, {
          kind: 'schema.move',
          subject: { type: 'schema', id: `${rec.info.name}/${m.name}` },
          steps: ['Check the move', 'Copy the data', 'Switch', 'Clean up the old copy'],
          cancellable: true,
          failWith:
            m.name === 'failme' ? 'the copy failed: connection to the target was lost' : undefined,
          onFail: () => {
            m.moving = false
          },
          apply: () => {
            m.moving = false
            m.group = b.targetShardGroupId
            m.keyless = []
          },
        }),
      )
    },
  }
}
