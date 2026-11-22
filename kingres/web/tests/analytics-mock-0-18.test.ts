// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Phase 12 in the mock (contract 0.18.0): the 11 new operations behave as the contract says. Every task is a 202 with the kind the
// contract names; a request the contract refuses is refused with its code and changes nothing; a second request for a subject a
// task holds is 409 operation_in_progress; the roles are enforced; `configureApproximateAggregates` and `putPartitionPolicy`
// are idempotent; `planPartitionPolicy` is a read and changes nothing.
import { beforeEach, describe, expect, it } from 'vitest'
import { waitFor } from '@testing-library/vue'
import { createPinia } from 'pinia'
import { endpoints } from '@/api/endpoints'
import { ApiError } from '@/api/problem'
import { getState } from '@/mocks/http'
import { mockConfig } from '@/mocks/state'
import { applySwitches } from '@/mocks/switches'
import { ok } from './schema-check'
import { signInAs } from './helpers'

const sw = (q: string) => applySwitches(new URLSearchParams(q), getState())
const shop = () => getState().databases.find((d) => d.info.name === 'shop')!
const settled = (id: number, state: 'done' | 'failed' = 'done') =>
  waitFor(async () => expect((await endpoints.getTask(id)).state).toBe(state), { timeout: 30_000 })
async function refused(promise: Promise<unknown>) {
  const error = await promise.then(
    () => null,
    (e: unknown) => e,
  )
  expect(error).toBeInstanceOf(ApiError)
  const e = error as ApiError
  return {
    status: e.problem.status,
    code: e.code,
    errors: e.problem.errors ?? [],
    detail: e.problem.detail,
  }
}
const partitions = () =>
  endpoints.listTablePartitions('shop', 'public', 'metrics').then((p) => p.items)

beforeEach(async () => {
  await signInAs(createPinia())
  mockConfig.taskDurationMs = 60
})

describe('approximate aggregates', () => {
  it('a database that never had it says off, with every server not installed', async () => {
    const a = await endpoints.getApproximateAggregates('shop')
    ok(a, 'ApproximateAggregates')
    expect(a).toMatchObject({
      enabled: false,
      extensionVersion: null,
      countDistinct: 'exact',
      errorRate: null,
    })
    expect(a.servers.length).toBeGreaterThan(0)
    expect(a.servers.every((s) => s.state === 'notInstalled' && s.installedVersion === null)).toBe(
      true,
    )
  })

  it('enabling answers a task of the contract kind and ends with the setting recorded', async () => {
    const task = await endpoints.configureApproximateAggregates('shop', {
      enabled: true,
      countDistinct: 'approximate',
      errorRate: 0.02,
    })
    ok(task, 'Task')
    expect(task.kind).toBe('database.approximateAggregates')
    expect(task.subject).toEqual({ type: 'database', id: 'shop' })
    expect(task.progress.steps.map((s) => s.name)).toEqual([
      'Check the servers',
      ...getState().servers.map((s) => `Install the extension on server ${s.id}`),
      'Verify versions and hashes',
      'Apply the database settings',
      'Record the settings',
    ])
    await settled(task.id)
    const a = await endpoints.getApproximateAggregates('shop')
    ok(a, 'ApproximateAggregates')
    expect(a).toMatchObject({
      enabled: true,
      extensionVersion: '1.0',
      countDistinct: 'approximate',
      errorRate: 0.02,
    })
    expect(a.servers.every((s) => s.state === 'ok' && s.installedVersion === '1.0')).toBe(true)
    expect((await endpoints.getDatabase('shop')).approximateAggregates).toMatchObject({
      enabled: true,
    })
  })

  it('is idempotent: the same request again is another task and the same state', async () => {
    const body = { enabled: true, countDistinct: 'approximate', errorRate: 0.01 } as const
    const first = await endpoints.configureApproximateAggregates('shop', body)
    await settled(first.id)
    const before = await endpoints.getApproximateAggregates('shop')
    const second = await endpoints.configureApproximateAggregates('shop', body)
    expect(second.id).not.toBe(first.id)
    await settled(second.id)
    expect(await endpoints.getApproximateAggregates('shop')).toEqual(before)
  })

  it('turning it off makes count(DISTINCT) exact and never takes the extension away', async () => {
    sw('analytics=on')
    const task = await endpoints.configureApproximateAggregates('shop', { enabled: false })
    // A disable runs the same five steps as an enable; the install steps find the extension installed and do nothing.
    expect(task.progress.steps.map((s) => s.name)).toEqual([
      'Check the servers',
      ...getState().servers.map((s) => `Install the extension on server ${s.id}`),
      'Verify versions and hashes',
      'Apply the database settings',
      'Record the settings',
    ])
    await settled(task.id)
    const a = await endpoints.getApproximateAggregates('shop')
    expect(a).toMatchObject({
      enabled: false,
      countDistinct: 'exact',
      errorRate: null,
      extensionVersion: '1.0',
    })
  })

  it('refuses an approximate mode that is off, and a rate out of range, with the field', async () => {
    const off = await refused(
      endpoints.configureApproximateAggregates('shop', {
        enabled: false,
        countDistinct: 'approximate',
      }),
    )
    expect(off.status).toBe(400)
    expect(off.code).toBe('validation_failed')
    expect(off.errors.map((e) => e.field)).toEqual(['countDistinct'])
    for (const errorRate of [0.0004, 0.11, 0]) {
      const bad = await refused(
        endpoints.configureApproximateAggregates('shop', {
          enabled: true,
          countDistinct: 'approximate',
          errorRate,
        }),
      )
      expect(bad.errors.map((e) => e.field)).toEqual(['errorRate'])
    }
    expect((await endpoints.getApproximateAggregates('shop')).enabled).toBe(false)
  })

  it('a server without the package is 422 extension_unavailable, two versions 409 extension_version_mismatch, and nothing changes', async () => {
    sw('analytics=sketch:missing')
    const missing = await refused(
      endpoints.configureApproximateAggregates('shop', { enabled: true }),
    )
    expect(missing).toMatchObject({ status: 422, code: 'extension_unavailable' })
    expect(missing.detail).toMatch(/has no package/)
    getState().analytics!.knobs.sketch = 'mismatch'
    const mismatch = await refused(
      endpoints.configureApproximateAggregates('shop', { enabled: true }),
    )
    expect(mismatch).toMatchObject({ status: 409, code: 'extension_version_mismatch' })
    expect((await endpoints.getApproximateAggregates('shop')).enabled).toBe(false)
    // Turning it off needs no package, so it is not refused.
    await endpoints.configureApproximateAggregates('shop', { enabled: false })
  })

  it('a hash that differs ends the task failed at the check, with sketch_hash_mismatch, and records nothing', async () => {
    sw('analytics=sketch:hash')
    const task = await endpoints.configureApproximateAggregates('shop', { enabled: true })
    await settled(task.id, 'failed')
    const t = await endpoints.getTask(task.id)
    expect(t.error?.code).toBe('sketch_hash_mismatch')
    expect((await endpoints.getApproximateAggregates('shop')).enabled).toBe(false)
  })

  it('another database task running is 409 operation_in_progress', async () => {
    mockConfig.taskDurationMs = 60_000
    await endpoints.configureApproximateAggregates('shop', { enabled: true })
    expect(
      (await refused(endpoints.configureApproximateAggregates('shop', { enabled: true }))).code,
    ).toBe('operation_in_progress')
    // Another database is not held.
    await endpoints.configureApproximateAggregates('reports', { enabled: true })
  })

  it('an unknown database is 404', async () => {
    expect((await refused(endpoints.getApproximateAggregates('nothing'))).status).toBe(404)
    expect(
      (await refused(endpoints.configureApproximateAggregates('nothing', { enabled: true })))
        .status,
    ).toBe(404)
  })
})

describe('the storage format of a table', () => {
  it('every table says its storage; a table that was never converted is a row table', async () => {
    const list = await endpoints.listTables('shop')
    for (const t of list.items) {
      ok(t, 'Table')
      expect(t.storage).toEqual({ accessMethod: 'heap', partitionFormats: null, columnar: null })
    }
    expect((await endpoints.getTable('shop', 'public', 'notes')).storage?.accessMethod).toBe('heap')
  })

  it('a table with indexes, a trigger or a ledger is refused with its reasons, and nothing changes', async () => {
    const notes = await refused(
      endpoints.setTableStorage('shop', 'public', 'notes', { accessMethod: 'columnar' }),
    )
    expect(notes).toMatchObject({ status: 409, code: 'conversion_refused' })
    expect(notes.detail).toMatch(/has an index \(notes_pkey\)/)
    const exports = await refused(
      endpoints.setTableStorage('reports', 'public', 'exports', { accessMethod: 'columnar' }),
    )
    expect(exports.detail).toMatch(/has a trigger \(exports_audit\)/)
    shop().tables.find((t) => t.name === 'notes')!.classes = ['ledger']
    const ledger = await refused(
      endpoints.setTableStorage('shop', 'public', 'notes', { accessMethod: 'columnar' }),
    )
    expect(ledger.detail).toMatch(/is a ledger table/)
    expect(ledger.detail?.split('\n').length).toBeGreaterThan(1)
    expect((await endpoints.getTable('shop', 'public', 'notes')).storage?.accessMethod).toBe('heap')
  })

  it('a table that is partitioned, or distributed, is refused', async () => {
    sw('analytics=timeseries')
    const parent = await refused(
      endpoints.setTableStorage('shop', 'public', 'metrics', { accessMethod: 'columnar' }),
    )
    expect(parent).toMatchObject({ code: 'conversion_refused' })
    expect(parent.detail).toMatch(/partition/)
    const dist = await refused(
      endpoints.setTableStorage('shop', 'public', 'customers', { accessMethod: 'columnar' }),
    )
    expect(dist.code).toBe('conversion_refused')
  })

  it('refuses a bad request field by field', async () => {
    sw('analytics=timeseries')
    const bad = await refused(
      endpoints.setTableStorage('shop', 'public', 'clicks', {
        accessMethod: 'columnar',
        compression: 'zstd',
        compressionLevel: 20,
        stripeRowCount: 10,
        chunkRowCount: 5,
      }),
    )
    expect(bad.status).toBe(400)
    expect(bad.errors.map((e) => e.field).sort()).toEqual([
      'chunkRowCount',
      'compressionLevel',
      'stripeRowCount',
    ])
    const method = await refused(
      endpoints.setTableStorage('shop', 'public', 'clicks', { accessMethod: 'tape' as never }),
    )
    expect(method.errors.map((e) => e.field)).toEqual(['accessMethod'])
  })

  it('converts a table that can be converted: a task with the three steps, the format and the size change, and back', async () => {
    sw('analytics=timeseries')
    const before = (await endpoints.getTable('shop', 'public', 'clicks')).sizeBytes as number
    const task = await endpoints.setTableStorage('shop', 'public', 'clicks', {
      accessMethod: 'columnar',
      compression: 'zstd',
      compressionLevel: 5,
    })
    ok(task, 'Task')
    expect(task.kind).toBe('table.setStorage')
    expect(task.subject).toEqual({ type: 'table', id: 'shop/public.clicks' })
    expect(task.progress.steps.map((s) => s.name)).toEqual([
      'Check the table',
      'Convert the storage',
      'Analyze the table',
    ])
    expect(task.detail).toMatch(/ACCESS EXCLUSIVE/)
    await settled(task.id)
    const t = await endpoints.getTable('shop', 'public', 'clicks')
    ok(t, 'TableDetail')
    expect(t.storage?.accessMethod).toBe('columnar')
    expect(t.storage?.columnar).toMatchObject({ compression: 'zstd', compressionLevel: 5 })
    expect(t.sizeBytes).toBeLessThan(before)
    expect(
      (await endpoints.listTables('shop', {})).items.find((x) => x.name === 'clicks')?.storage
        ?.accessMethod,
    ).toBe('columnar')
    const back = await endpoints.setTableStorage('shop', 'public', 'clicks', {
      accessMethod: 'heap',
    })
    await settled(back.id)
    const h = await endpoints.getTable('shop', 'public', 'clicks')
    expect(h.storage).toEqual({ accessMethod: 'heap', partitionFormats: null, columnar: null })
    expect(h.sizeBytes).toBe(before)
  })

  it('without the extension, 409 extension_missing for columnar', async () => {
    sw('analytics=timeseries,columnar:missing')
    const e = await refused(
      endpoints.setTableStorage('shop', 'public', 'clicks', { accessMethod: 'columnar' }),
    )
    expect(e).toMatchObject({ status: 409, code: 'extension_missing' })
    expect(e.detail).toMatch(/kingres_columnar/)
  })

  it('a second change of the table while one runs is 409 operation_in_progress; a failed one leaves the table as it was', async () => {
    sw('analytics=timeseries')
    mockConfig.taskDurationMs = 60_000
    await endpoints.setTableStorage('shop', 'public', 'clicks', { accessMethod: 'columnar' })
    expect(
      (
        await refused(
          endpoints.setTableStorage('shop', 'public', 'clicks', { accessMethod: 'columnar' }),
        )
      ).code,
    ).toBe('operation_in_progress')
  })

  it('a conversion that fails ends the task failed and the table is unchanged', async () => {
    sw('analytics=timeseries,convert:fail')
    const task = await endpoints.setTableStorage('shop', 'public', 'clicks', {
      accessMethod: 'columnar',
    })
    await settled(task.id, 'failed')
    expect((await endpoints.getTask(task.id)).error?.code).toBe('insufficient_space')
    expect((await endpoints.getTable('shop', 'public', 'clicks')).storage?.accessMethod).toBe(
      'heap',
    )
  })

  it('a cancelled conversion leaves the table as it was', async () => {
    sw('analytics=timeseries')
    mockConfig.taskDurationMs = 60_000
    const task = await endpoints.setTableStorage('shop', 'public', 'clicks', {
      accessMethod: 'columnar',
    })
    expect(task.cancellable).toBe(true)
    await endpoints.cancelTask(task.id)
    expect((await endpoints.getTask(task.id)).error?.code).toBe('cancelled')
    expect((await endpoints.getTable('shop', 'public', 'clicks')).storage?.accessMethod).toBe(
      'heap',
    )
  })

  it('the storage filter of the list: a partitioned table matches columnar when any partition is', async () => {
    sw('analytics=timeseries')
    const names = async (storage?: 'heap' | 'columnar') =>
      (await endpoints.listTables('shop', storage ? ({ storage } as never) : {})).items.map(
        (x) => x.name,
      )
    expect(await names()).toContain('metrics')
    expect(await names('columnar')).toEqual(['metrics'])
    expect(await names('heap')).toContain('clicks')
    expect(await names('heap')).toContain('metrics')
    const bad = await refused(endpoints.listTables('shop', { storage: 'tape' } as never))
    expect(bad.status).toBe(400)
    const t = await endpoints.getTable('shop', 'public', 'metrics')
    expect(t.storage?.accessMethod).toBeNull()
    expect(t.storage?.partitionFormats).toEqual({ heap: 8, columnar: 2, other: 0 })
    expect(t.storage?.columnar?.compressionRatio).toBe(6.2)
    expect(t.partitioning).toMatchObject({
      columnType: 'timestamptz',
      hasPolicy: true,
      defaultPartition: false,
    })
  })
})

describe('the storage format of a partition', () => {
  beforeEach(() => sw('analytics=timeseries'))

  it('converts one partition aside: the task has the partition as subject, and the list shows the format and the ratio', async () => {
    const list = await partitions()
    const heap = list.find((p) => p.accessMethod === 'heap')!
    const task = await endpoints.setPartitionStorage('shop', 'public', 'metrics', heap.name, {
      accessMethod: 'columnar',
      sortBy: ['ts'],
    })
    ok(task, 'Task')
    expect(task.kind).toBe('table.setStorage')
    expect(task.subject).toEqual({ type: 'table', id: `shop/public.${heap.name}` })
    await settled(task.id)
    const after = (await partitions()).find((p) => p.name === heap.name)!
    ok(after, 'TablePartition')
    expect(after.accessMethod).toBe('columnar')
    expect(after.compressionRatio).toBe(6.2)
    expect(after.sizeBytes).toBeLessThan(heap.sizeBytes as number)
  })

  it('refuses a missing partition (404), a sort column the table does not have (400), the same format, and a publication', async () => {
    expect(
      (
        await refused(
          endpoints.setPartitionStorage('shop', 'public', 'metrics', 'nope', {
            accessMethod: 'columnar',
          }),
        )
      ).status,
    ).toBe(404)
    const heap = (await partitions()).find((p) => p.accessMethod === 'heap')!
    const sort = await refused(
      endpoints.setPartitionStorage('shop', 'public', 'metrics', heap.name, {
        accessMethod: 'columnar',
        sortBy: ['missing'],
      }),
    )
    expect(sort).toMatchObject({ status: 400, code: 'validation_failed' })
    expect(sort.errors.map((e) => e.field)).toEqual(['sortBy'])
    const same = await refused(
      endpoints.setPartitionStorage('shop', 'public', 'metrics', heap.name, {
        accessMethod: 'heap',
      }),
    )
    expect(same.code).toBe('conversion_refused')
    getState().analytics!.knobs.publication = true
    const pub = await refused(
      endpoints.setPartitionStorage('shop', 'public', 'metrics', heap.name, {
        accessMethod: 'columnar',
      }),
    )
    expect(pub.detail).toMatch(/publication/)
  })

  it('refuses a default partition and the extensions that are missing', async () => {
    const list = await partitions()
    const def = {
      ...list[0]!,
      name: 'metrics_default',
      bound: 'DEFAULT',
      isDefault: true,
      rangeFrom: null,
      rangeTo: null,
    }
    getState().partitions!['shop/public.metrics'] = [...list, def]
    const d = await refused(
      endpoints.setPartitionStorage('shop', 'public', 'metrics', 'metrics_default', {
        accessMethod: 'columnar',
      }),
    )
    expect(d.detail).toMatch(/default partition/)
    const heap = list.find((p) => p.accessMethod === 'heap')!
    getState().analytics!.knobs.timepartMissing = true
    expect(
      (
        await refused(
          endpoints.setPartitionStorage('shop', 'public', 'metrics', heap.name, {
            accessMethod: 'columnar',
          }),
        )
      ).code,
    ).toBe('extension_missing')
    getState().analytics!.knobs.timepartMissing = false
    getState().analytics!.knobs.columnarMissing = true
    expect(
      (
        await refused(
          endpoints.setPartitionStorage('shop', 'public', 'metrics', heap.name, {
            accessMethod: 'columnar',
          }),
        )
      ).code,
    ).toBe('extension_missing')
  })

  it('a partition that is being converted holds itself and the policy run of the table', async () => {
    mockConfig.taskDurationMs = 60_000
    const heap = (await partitions()).find((p) => p.accessMethod === 'heap')!
    await endpoints.setPartitionStorage('shop', 'public', 'metrics', heap.name, {
      accessMethod: 'columnar',
    })
    expect(
      (
        await refused(
          endpoints.setPartitionStorage('shop', 'public', 'metrics', heap.name, {
            accessMethod: 'columnar',
          }),
        )
      ).code,
    ).toBe('operation_in_progress')
  })
})

describe('the time-partitioning policy', () => {
  const policy = {
    partitionEvery: 'P1D',
    createAhead: 'P7D',
    retention: 'P90D',
    retentionAction: 'drop',
    columnarAfter: 'P4D',
    compression: 'zstd',
    compressionLevel: 3,
    timezone: 'UTC',
    runEvery: 'PT1H',
    enabled: true,
  } as const

  it('a table without a policy is 404 not_found', async () => {
    sw('analytics=timeseries')
    await endpoints.deletePartitionPolicy('shop', 'public', 'metrics')
    expect((await refused(endpoints.getPartitionPolicy('shop', 'public', 'metrics'))).status).toBe(
      404,
    )
    expect(
      (await refused(endpoints.deletePartitionPolicy('shop', 'public', 'metrics'))).status,
    ).toBe(404)
    expect((await endpoints.getTable('shop', 'public', 'metrics')).partitioning?.hasPolicy).toBe(
      false,
    )
  })

  it('reads the policy with its health, its last run and its next run', async () => {
    sw('analytics=timeseries')
    const p = await endpoints.getPartitionPolicy('shop', 'public', 'metrics')
    ok(p, 'PartitionPolicy')
    expect(p).toMatchObject({
      partitionColumn: 'ts',
      partitionColumnType: 'timestamptz',
      partitionEvery: 'P1D',
      createdBy: 'admin',
    })
    expect(p.health.partitionsAhead).toBe('P2D')
    expect(p.health.problems).toContain('aheadLow')
    expect(p.lastRun?.state).toBe('done')
    expect(Date.parse(p.nextRunAt as string)).toBeGreaterThan(Date.now() - 1000)
  })

  it('put is synchronous (200), stores the policy, creates no partition, and is idempotent', async () => {
    sw('analytics=timeseries')
    await endpoints.deletePartitionPolicy('shop', 'public', 'metrics')
    const count = (await partitions()).length
    const first = await endpoints.putPartitionPolicy('shop', 'public', 'metrics', policy)
    ok(first, 'PartitionPolicy')
    expect(Math.abs(Date.parse(first.nextRunAt as string) - Date.now())).toBeLessThan(5000)
    expect((await partitions()).length).toBe(count)
    const second = await endpoints.putPartitionPolicy('shop', 'public', 'metrics', policy)
    expect({ ...second, updatedAt: '', nextRunAt: '' }).toEqual({
      ...first,
      updatedAt: '',
      nextRunAt: '',
    })
    expect(await endpoints.getPartitionPolicy('shop', 'public', 'metrics')).toMatchObject({
      retention: 'P90D',
    })
  })

  it('refuses every field it cannot take, with the JSON pointer, and stores nothing', async () => {
    sw('analytics=timeseries')
    const bad = await refused(
      endpoints.putPartitionPolicy('shop', 'public', 'metrics', {
        partitionEvery: 'P7X',
        createAhead: 'tomorrow',
        retention: 'P1D',
        columnarAfter: 'P2D',
        compressionLevel: 99,
        timezone: 'Nowhere/Land',
        runEvery: 'PT1H',
      }),
    )
    expect(bad).toMatchObject({ status: 400, code: 'validation_failed' })
    expect(bad.errors.map((e) => e.field).sort()).toEqual(
      [
        '/columnarAfter',
        '/compressionLevel',
        '/createAhead',
        '/partitionEvery',
        '/timezone',
      ].sort(),
    )
    const order = await refused(
      endpoints.putPartitionPolicy('shop', 'public', 'metrics', {
        ...policy,
        retention: 'P2D',
        columnarAfter: 'P10D',
      }),
    )
    expect(order.errors.map((e) => e.field)).toEqual(['/columnarAfter'])
    const shapes = await refused(
      endpoints.putPartitionPolicy('shop', 'public', 'metrics', {
        ...policy,
        partitionEvery: 'PT5H',
      }),
    )
    expect(shapes.errors.map((e) => e.field)).toEqual(['/partitionEvery'])
    expect((await endpoints.getPartitionPolicy('shop', 'public', 'metrics')).retention).toBe('P90D')
  })

  it('a table that is not range-partitioned on a time column is 409 partition_policy_conflict; no extension is 409 extension_missing', async () => {
    applySwitches(new URLSearchParams('analytics=timeseries&tables=partitions'), getState())
    // Partitioned by range, but on a column that is a bigint.
    const t = shop().tables.find((x) => x.name === 'measurements')!
    t.partitioning = { method: 'range', key: 'sensor_id', partitionCount: 3 }
    expect(
      (await refused(endpoints.putPartitionPolicy('shop', 'public', 'measurements', policy))).code,
    ).toBe('partition_policy_conflict')
    expect(
      (await refused(endpoints.putPartitionPolicy('shop', 'public', 'notes', policy))).code,
    ).toBe('partition_policy_conflict')
    getState().analytics!.knobs.timepartMissing = true
    expect(
      (await refused(endpoints.putPartitionPolicy('shop', 'public', 'metrics', policy))).code,
    ).toBe('extension_missing')
  })

  it('plan is a read: it lists what a run would create, convert and drop, and changes nothing', async () => {
    sw('analytics=timeseries')
    const before = JSON.stringify(await partitions())
    const plan = await endpoints.planPartitionPolicy('shop', 'public', 'metrics')
    ok(plan, 'PartitionPlan')
    expect(plan.actions.length).toBeGreaterThan(0)
    expect(plan.actions.every((a) => a.state === 'planned' || a.state === 'skipped')).toBe(true)
    expect(plan.actions.map((a) => a.seq)).toEqual(plan.actions.map((_, i) => i + 1))
    // 3 days ahead exist, 7 are wanted: the partitions of the next five days are created, and the one that is old enough is converted.
    expect(plan.actions.filter((a) => a.kind === 'create')).toHaveLength(5)
    expect(plan.actions.filter((a) => a.kind === 'convert')).toHaveLength(1)
    expect(JSON.stringify(await partitions())).toBe(before)
    expect(getState().audit.some((e) => e.action === ('planPartitionPolicy' as never))).toBe(false)
  })

  it('plan with the policy of the request shows what a retention would drop, with sizes, before saving', async () => {
    sw('analytics=timeseries')
    const plan = await endpoints.planPartitionPolicy('shop', 'public', 'metrics', {
      ...policy,
      retention: 'P3D',
      columnarAfter: null,
    })
    const drops = plan.actions.filter((a) => a.kind === 'drop')
    expect(drops.length).toBeGreaterThanOrEqual(3)
    expect(drops.every((a) => (a.bytesBefore ?? 0) > 0 && a.bytesAfter === null)).toBe(true)
    const detach = await endpoints.planPartitionPolicy('shop', 'public', 'metrics', {
      ...policy,
      retention: 'P3D',
      retentionAction: 'detach',
      columnarAfter: null,
    })
    expect(detach.actions.some((a) => a.kind === 'detach')).toBe(true)
    expect(detach.actions.some((a) => a.kind === 'drop')).toBe(false)
    expect((await endpoints.getPartitionPolicy('shop', 'public', 'metrics')).retention).toBe('P90D')
    const invalid = await refused(
      endpoints.planPartitionPolicy('shop', 'public', 'metrics', {
        ...policy,
        partitionEvery: 'nope',
      }),
    )
    expect(invalid.status).toBe(400)
  })

  it('plan of a table with no policy and none in the request is 404', async () => {
    sw('analytics=timeseries')
    await endpoints.deletePartitionPolicy('shop', 'public', 'metrics')
    expect((await refused(endpoints.planPartitionPolicy('shop', 'public', 'metrics'))).status).toBe(
      404,
    )
    expect(
      (await endpoints.planPartitionPolicy('shop', 'public', 'metrics', policy)).actions.length,
    ).toBeGreaterThan(0)
  })

  it('run is a 202 task partition.maintain with the five steps; the partitions change and the run is recorded as manual', async () => {
    sw('analytics=timeseries')
    const before = (await partitions()).length
    const task = await endpoints.runPartitionPolicy('shop', 'public', 'metrics')
    ok(task, 'Task')
    expect(task.kind).toBe('partition.maintain')
    expect(task.subject).toEqual({ type: 'table', id: 'shop/public.metrics' })
    expect(task.progress.steps.map((s) => s.name)).toEqual([
      'Begin the run',
      'Create partitions',
      'Convert partitions',
      'Retire partitions',
      'Finish the run',
    ])
    const running = await endpoints.listPartitionPolicyRuns('shop', 'public', 'metrics')
    expect(running.items[0]).toMatchObject({
      state: 'running',
      trigger: 'manual',
      taskId: task.id,
      finishedAt: null,
    })
    await settled(task.id)
    expect((await partitions()).length).toBe(before + 5)
    const runs = await endpoints.listPartitionPolicyRuns('shop', 'public', 'metrics')
    ok(runs, 'PartitionRunPage')
    expect(runs.items[0]).toMatchObject({ state: 'done', trigger: 'manual', startedBy: 'admin' })
    expect(runs.items[0]!.actions).toEqual([])
    const run = await endpoints.getPartitionPolicyRun(
      'shop',
      'public',
      'metrics',
      runs.items[0]!.id,
    )
    ok(run, 'PartitionRun')
    expect(run.actions.filter((a) => a.kind === 'create' && a.state === 'done')).toHaveLength(5)
    // A second run finds nothing more to create.
    const again = await endpoints.runPartitionPolicy('shop', 'public', 'metrics')
    await settled(again.id)
    const second = await endpoints.getPartitionPolicyRun(
      'shop',
      'public',
      'metrics',
      (await endpoints.listPartitionPolicyRuns('shop', 'public', 'metrics')).items[0]!.id,
    )
    expect(second.actions.filter((a) => a.kind === 'create')).toHaveLength(0)
  })

  it('a run that waits for a lock is partial, with the detail on the action, and an alert', async () => {
    sw('analytics=timeseries,run:lock')
    const task = await endpoints.runPartitionPolicy('shop', 'public', 'metrics')
    await settled(task.id)
    const run = (await endpoints.listPartitionPolicyRuns('shop', 'public', 'metrics')).items[0]!
    expect(run.state).toBe('partial')
    expect(run.error).toMatch(/did not finish/)
    const detail = await endpoints.getPartitionPolicyRun('shop', 'public', 'metrics', run.id)
    expect(detail.actions.find((a) => a.state === 'failed')).toMatchObject({
      kind: 'convert',
      detail: 'lock_timeout',
    })
    expect(
      getState().alerts.some((a) => a.kind === 'partitionPolicyFailed' && a.state === 'open'),
    ).toBe(true)
  })

  it('a run that fails ends the task failed and the run failed; a second run while one goes on is 409', async () => {
    sw('analytics=timeseries,run:fail')
    const task = await endpoints.runPartitionPolicy('shop', 'public', 'metrics')
    await settled(task.id, 'failed')
    const run = (await endpoints.listPartitionPolicyRuns('shop', 'public', 'metrics')).items[0]!
    expect(run.state).toBe('failed')
    expect(run.error).toMatch(/lock/)
    getState().analytics!.knobs.runFails = false
    mockConfig.taskDurationMs = 60_000
    await endpoints.runPartitionPolicy('shop', 'public', 'metrics')
    expect((await refused(endpoints.runPartitionPolicy('shop', 'public', 'metrics'))).code).toBe(
      'operation_in_progress',
    )
    expect((await refused(endpoints.deletePartitionPolicy('shop', 'public', 'metrics'))).code).toBe(
      'operation_in_progress',
    )
  })

  it('a table with no policy cannot run one (404); the runs list filters by state and pages', async () => {
    sw('analytics=timeseries')
    const all = await endpoints.listPartitionPolicyRuns('shop', 'public', 'metrics')
    expect(all.items.map((r) => r.state)).toEqual(['done', 'partial', 'done'])
    const partial = await endpoints.listPartitionPolicyRuns('shop', 'public', 'metrics', {
      state: 'partial',
    })
    expect(partial.items.map((r) => r.state)).toEqual(['partial'])
    const page = await endpoints.listPartitionPolicyRuns('shop', 'public', 'metrics', { limit: 2 })
    expect(page.items).toHaveLength(2)
    expect(page.nextCursor).not.toBeNull()
    expect(
      (await refused(endpoints.getPartitionPolicyRun('shop', 'public', 'metrics', 9999))).status,
    ).toBe(404)
    await endpoints.deletePartitionPolicy('shop', 'public', 'metrics')
    expect((await refused(endpoints.runPartitionPolicy('shop', 'public', 'metrics'))).status).toBe(
      404,
    )
  })

  it('the policy of a table that has one, with a retention, drops the old partitions at the run', async () => {
    sw('analytics=timeseries')
    await endpoints.putPartitionPolicy('shop', 'public', 'metrics', {
      ...policy,
      retention: 'P3D',
      columnarAfter: null,
    })
    const names = (await partitions()).map((p) => p.name)
    const task = await endpoints.runPartitionPolicy('shop', 'public', 'metrics')
    await settled(task.id)
    const after = (await partitions()).map((p) => p.name)
    expect(names.filter((n) => !after.includes(n)).length).toBeGreaterThanOrEqual(3)
  })

  it('a default partition is shown by the table and by the health', async () => {
    sw('analytics=timeseries')
    const list = await partitions()
    getState().partitions!['shop/public.metrics'] = [
      ...list,
      {
        ...list[0]!,
        name: 'metrics_default',
        bound: 'DEFAULT',
        isDefault: true,
        rangeFrom: null,
        rangeTo: null,
      },
    ]
    expect(
      (await endpoints.getTable('shop', 'public', 'metrics')).partitioning?.defaultPartition,
    ).toBe(true)
    expect(
      (await endpoints.getPartitionPolicy('shop', 'public', 'metrics')).health.problems,
    ).toContain('defaultPartitionPresent')
  })

  it('the alert partitionsAheadLow is raised with the table', async () => {
    sw('analytics=ahead-low')
    const alert = getState().alerts.find((a) => a.kind === 'partitionsAheadLow')
    expect(alert?.subject).toMatchObject({
      databaseName: 'shop',
      tableSchema: 'public',
      tableName: 'metrics',
      aspect: null,
    })
    expect(
      (await endpoints.getPartitionPolicy('shop', 'public', 'metrics')).health.problems,
    ).toContain('aheadLow')
  })
})

describe('roles and older servers', () => {
  it.each([
    ['viewer', 'configureApproximateAggregates'],
    ['ops', 'configureApproximateAggregates'],
    ['viewer', 'setTableStorage'],
    ['ops', 'putPartitionPolicy'],
    ['viewer', 'runPartitionPolicy'],
    ['viewer', 'deletePartitionPolicy'],
  ])('%s gets 403 on %s and nothing changes', async (user, op) => {
    sw('analytics=timeseries')
    await signInAs(createPinia(), user)
    const call = {
      configureApproximateAggregates: () =>
        endpoints.configureApproximateAggregates('shop', { enabled: true }),
      setTableStorage: () =>
        endpoints.setTableStorage('shop', 'public', 'clicks', { accessMethod: 'columnar' }),
      putPartitionPolicy: () =>
        endpoints.putPartitionPolicy('shop', 'public', 'metrics', { partitionEvery: 'P1D' }),
      runPartitionPolicy: () => endpoints.runPartitionPolicy('shop', 'public', 'metrics'),
      deletePartitionPolicy: () => endpoints.deletePartitionPolicy('shop', 'public', 'metrics'),
    }[op as 'setTableStorage']
    expect((await refused(call())).status).toBe(403)
    expect(
      getState().tasks.some((t) =>
        ['database.approximateAggregates', 'table.setStorage', 'partition.maintain'].includes(
          t.task.kind,
        ),
      ),
    ).toBe(false)
    expect(getState().analytics?.policies['shop/public.metrics']).toBeDefined()
  })

  it('a read-only role reads the card, the policy, the runs and the plan', async () => {
    sw('analytics=timeseries')
    await signInAs(createPinia(), 'viewer')
    expect((await endpoints.getApproximateAggregates('shop')).enabled).toBe(false)
    expect((await endpoints.getPartitionPolicy('shop', 'public', 'metrics')).partitionEvery).toBe(
      'P1D',
    )
    expect(
      (await endpoints.listPartitionPolicyRuns('shop', 'public', 'metrics')).items.length,
    ).toBeGreaterThan(0)
    expect(
      (await endpoints.planPartitionPolicy('shop', 'public', 'metrics')).actions.length,
    ).toBeGreaterThan(0)
  })

  it('a server of an older revision answers 422 unsupported_operation on every new operation', async () => {
    sw('analytics=old,timeseries')
    const calls = [
      () => endpoints.getApproximateAggregates('shop'),
      () => endpoints.configureApproximateAggregates('shop', { enabled: true }),
      () => endpoints.setTableStorage('shop', 'public', 'clicks', { accessMethod: 'columnar' }),
      () =>
        endpoints.setPartitionStorage('shop', 'public', 'metrics', 'p', {
          accessMethod: 'columnar',
        }),
      () => endpoints.getPartitionPolicy('shop', 'public', 'metrics'),
      () => endpoints.putPartitionPolicy('shop', 'public', 'metrics', { partitionEvery: 'P1D' }),
      () => endpoints.deletePartitionPolicy('shop', 'public', 'metrics'),
      () => endpoints.runPartitionPolicy('shop', 'public', 'metrics'),
      () => endpoints.listPartitionPolicyRuns('shop', 'public', 'metrics'),
      () => endpoints.getPartitionPolicyRun('shop', 'public', 'metrics', 1),
      () => endpoints.planPartitionPolicy('shop', 'public', 'metrics'),
    ]
    expect(calls).toHaveLength(11)
    for (const call of calls)
      expect(await refused(call())).toMatchObject({ status: 422, code: 'unsupported_operation' })
  })

  it('the audit log records the mutations, and the plan is not audited', async () => {
    sw('analytics=timeseries')
    await endpoints.planPartitionPolicy('shop', 'public', 'metrics')
    await endpoints.putPartitionPolicy('shop', 'public', 'metrics', { partitionEvery: 'P1D' })
    await endpoints.configureApproximateAggregates('shop', { enabled: false })
    const actions = getState().audit.map((e) => e.action)
    expect(actions).toContain('putPartitionPolicy')
    expect(actions).toContain('configureApproximateAggregates')
    expect(actions).not.toContain('planPartitionPolicy')
  })
})

describe('the workbench EXPLAIN of the mock', () => {
  const explain = async (sql: string) => {
    const r = await endpoints.executeQuery({ database: 'shop', sql })
    expect(r.columns[0]?.name).toBe('QUERY PLAN')
    return r.rows.map((x) => String(x[0]))
  }
  it('prints the lines the extension prints', async () => {
    expect(
      (
        await explain(
          'explain select * from orders o join events e on o.customer_id = e.customer_id',
        )
      ).join('\n'),
    ).toMatch(/repartitioned/)
    expect(
      (
        await explain(
          'explain select * from orders o join customers c on o.customer_id = c.customer_id',
        )
      ).join('\n'),
    ).toMatch(/Pushed: join/)
    expect(
      (await explain('explain select region, sum(total) from orders group by region')).join('\n'),
    ).toMatch(/aggregate \(partial\)/)
    expect((await explain('explain (verbose) select * from orders')).join('\n')).toMatch(
      /Pushdown: not used/,
    )
  })
  it('rewrites count(DISTINCT) only when the database has the approximate mode on', async () => {
    const sql = 'explain select count(distinct customer_id) from orders'
    expect((await explain(sql)).join('\n')).not.toMatch(/Rewritten/)
    sw('analytics=on')
    expect((await explain(sql)).join('\n')).toMatch(
      /Rewritten: count\(DISTINCT customer_id\) -> approximate \(p = 14\)/,
    )
  })
})

describe('the settled contract of 0.18.0 (review of the control plane)', () => {
  const policy = { partitionEvery: 'P1D', createAhead: 'P7D' } as const
  const dry = (db: string, table: string, body: object, partition?: string) =>
    fetch(
      `/api/v1/databases/${db}/schemas/public/tables/${table}${partition ? `/partitions/${partition}` : ''}/storage?dryRun=true`,
      {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      },
    )
  void dry

  it('dryRun answers 200 TableStoragePreview with the typed blockers and the notes, and starts no task', async () => {
    sw('analytics=timeseries')
    const ok1 = await endpoints.previewTableStorage('shop', 'public', 'clicks', {
      accessMethod: 'columnar',
    })
    ok(ok1, 'TableStoragePreview')
    expect(ok1).toMatchObject({
      current: 'heap',
      target: 'columnar',
      willConvert: true,
      blockers: [],
    })
    expect(ok1.notes.join(' ')).toMatch(/ACCESS EXCLUSIVE/)
    const notes = await endpoints.previewTableStorage('shop', 'public', 'notes', {
      accessMethod: 'columnar',
    })
    expect(notes.willConvert).toBe(false)
    expect(notes.blockers.map((b) => b.code)).toEqual(['other'])
    expect(notes.blockers[0]!.message).toMatch(/notes_pkey/)
    const parent = await endpoints.previewTableStorage('shop', 'public', 'metrics', {
      accessMethod: 'columnar',
    })
    expect(parent.current).toBeNull()
    expect(parent.blockers.map((b) => b.code)).toContain('partitioned_table')
    const dist = await endpoints.previewTableStorage('shop', 'public', 'customers', {
      accessMethod: 'columnar',
    })
    expect(dist.blockers.map((b) => b.code)).toContain('distributed_table')
    const same = await endpoints.previewTableStorage('shop', 'public', 'clicks', {
      accessMethod: 'heap',
    })
    expect(same).toMatchObject({ willConvert: false })
    expect(same.notes.join(' ')).toMatch(/stored as rows already/)
    expect(getState().tasks.some((t) => t.task.kind === 'table.setStorage')).toBe(false)
    expect(getState().audit.some((e) => e.action === 'setTableStorage')).toBe(false)
  })

  it('dryRun of a partition says the swap, and its blockers (a default partition, a publication)', async () => {
    sw('analytics=timeseries')
    const heap = (await partitions()).find((p) => p.accessMethod === 'heap')!
    const p = await endpoints.previewPartitionStorage('shop', 'public', 'metrics', heap.name, {
      accessMethod: 'columnar',
    })
    expect(p).toMatchObject({ current: 'heap', willConvert: true })
    getState().analytics!.knobs.publication = true
    const pub = await endpoints.previewPartitionStorage('shop', 'public', 'metrics', heap.name, {
      accessMethod: 'columnar',
    })
    expect(pub.blockers.map((b) => b.code)).toEqual(['publication'])
  })

  it('a dryRun is refused for the same extension and validation reasons as the change', async () => {
    sw('analytics=timeseries,columnar:missing')
    expect(
      (
        await refused(
          endpoints.previewTableStorage('shop', 'public', 'clicks', { accessMethod: 'columnar' }),
        )
      ).code,
    ).toBe('extension_missing')
    getState().analytics!.knobs.columnarMissing = false
    expect(
      (
        await refused(
          endpoints.previewTableStorage('shop', 'public', 'clicks', {
            accessMethod: 'columnar',
            compressionLevel: 99,
          }),
        )
      ).status,
    ).toBe(400)
  })

  it('conversion_refused carries the typed blockers, and the detail repeats them as text', async () => {
    sw('analytics=timeseries,publication')
    const error = await endpoints
      .setTableStorage('shop', 'public', 'clicks', { accessMethod: 'columnar' })
      .then(
        () => null,
        (e: unknown) => e as ApiError,
      )
    expect(error?.problem.blockers).toEqual([
      { code: 'publication', object: 'public.clicks', message: 'is in a publication' },
    ])
    expect(error?.problem.detail).toBe('is in a publication')
  })

  it('runPartitionPolicy: asOf must be within five minutes of now (400 on /asOf), the plan keeps a free asOf', async () => {
    sw('analytics=timeseries')
    const old = new Date(Date.now() - 3_600_000).toISOString()
    const bad = await refused(
      endpoints.runPartitionPolicy('shop', 'public', 'metrics', { asOf: old }),
    )
    expect(bad).toMatchObject({ status: 400, code: 'validation_failed' })
    expect(bad.errors.map((e) => e.field)).toEqual(['/asOf'])
    expect(getState().tasks.some((t) => t.task.kind === 'partition.maintain')).toBe(false)
    const near = new Date(Date.now() - 60_000).toISOString()
    const task = await endpoints.runPartitionPolicy('shop', 'public', 'metrics', { asOf: near })
    expect(task.kind).toBe('partition.maintain')
    const plan = await endpoints.planPartitionPolicy('shop', 'public', 'metrics')
    expect(plan.actions.length).toBeGreaterThan(0)
  })

  it('putPartitionPolicy is 409 operation_in_progress while a run of the policy goes on', async () => {
    sw('analytics=timeseries')
    mockConfig.taskDurationMs = 60_000
    await endpoints.runPartitionPolicy('shop', 'public', 'metrics')
    expect(
      (await refused(endpoints.putPartitionPolicy('shop', 'public', 'metrics', policy))).code,
    ).toBe('operation_in_progress')
  })

  it('getPartitionPolicy: 404 with a text of its own when kingres_timepart is not installed; the runs are an empty page', async () => {
    sw('analytics=timeseries,timepart:missing')
    const e = await refused(endpoints.getPartitionPolicy('shop', 'public', 'metrics'))
    expect(e.status).toBe(404)
    expect(e.detail).toMatch(/kingres_timepart is not installed/)
    expect((await endpoints.listPartitionPolicyRuns('shop', 'public', 'metrics')).items).toEqual([])
    const none = await refused(endpoints.getPartitionPolicy('shop', 'public', 'nothing'))
    expect(none.detail).toMatch(/does not exist/)
  })

  it('extension_missing says the remedy: restart the instance of the named server, there is no install action', async () => {
    sw('analytics=timeseries,columnar:missing')
    const e = await refused(
      endpoints.setTableStorage('shop', 'public', 'clicks', { accessMethod: 'columnar' }),
    )
    expect(e.detail).toMatch(/Restart the instance of that server/)
    expect(e.detail).toMatch(/no install action/)
  })

  it('the shapes of partitionEvery of the contract', async () => {
    sw('analytics=timeseries')
    const put = (partitionEvery: string) =>
      endpoints.putPartitionPolicy('shop', 'public', 'metrics', { partitionEvery })
    for (const good of [
      'PT1H',
      'PT12H',
      'P1D',
      'P7D',
      'P3660D',
      'P1M',
      'P3M',
      'P6M',
      'P1Y',
      'P1000Y',
    ])
      expect((await put(good)).partitionEvery).toBe(good)
    for (const bad of ['PT5H', 'PT24H', 'P3661D', 'P1W', 'P5M', 'P12M', 'P1001Y', 'P1DT1H'])
      expect(
        (await refused(put(bad))).errors.map((x) => x.field),
        bad,
      ).toEqual(['/partitionEvery'])
  })
})

describe('the owners of the tables (contract 0.18.0, items 14 to 16)', () => {
  const policy = { partitionEvery: 'P1D' } as const
  it('the cluster setting is off by default, and an update needs a boolean', async () => {
    expect((await endpoints.getClusterSettings()).analytics).toEqual({
      allowNonSuperuserOwners: false,
    })
    const bad = await refused(endpoints.updateClusterSettings({ analytics: {} } as never))
    expect(bad.status).toBe(400)
    const on = await endpoints.updateClusterSettings({
      analytics: { allowNonSuperuserOwners: true },
    })
    expect(on.analytics?.allowNonSuperuserOwners).toBe(true)
  })

  it('a table owned by a superuser is trusted: the owner is shown and nothing is refused', async () => {
    sw('analytics=timeseries')
    const p = await endpoints.getPartitionPolicy('shop', 'public', 'metrics')
    expect(p).toMatchObject({ tableOwner: 'postgres', ownerTrusted: true })
    const pre = await endpoints.previewTableStorage('shop', 'public', 'clicks', {
      accessMethod: 'columnar',
    })
    expect(pre).toMatchObject({ tableOwner: 'postgres', ownerTrusted: true })
  })

  it('an owner that is not trusted: the preview and the 409 carry owner_not_trusted, a policy put and a run are 409 partition_policy_conflict', async () => {
    sw('analytics=timeseries,owner:untrusted')
    const pre = await endpoints.previewTableStorage('shop', 'public', 'clicks', {
      accessMethod: 'columnar',
    })
    expect(pre).toMatchObject({ tableOwner: 'analyst', ownerTrusted: false, willConvert: false })
    expect(pre.blockers.map((b) => b.code)).toEqual(['owner_not_trusted'])
    const e = await endpoints
      .setTableStorage('shop', 'public', 'clicks', { accessMethod: 'columnar' })
      .then(
        () => null,
        (x: unknown) => x as ApiError,
      )
    expect(e?.code).toBe('conversion_refused')
    expect(e?.problem.blockers?.map((b) => b.code)).toEqual(['owner_not_trusted'])
    const heap = (await partitions()).find((x) => x.accessMethod === 'heap')!
    const part = await endpoints.previewPartitionStorage('shop', 'public', 'metrics', heap.name, {
      accessMethod: 'columnar',
    })
    expect(part.blockers.map((b) => b.code)).toContain('owner_not_trusted')
    expect(
      (await refused(endpoints.putPartitionPolicy('shop', 'public', 'metrics', policy))).code,
    ).toBe('partition_policy_conflict')
    expect((await refused(endpoints.runPartitionPolicy('shop', 'public', 'metrics'))).code).toBe(
      'partition_policy_conflict',
    )
    expect(getState().tasks.some((t) => t.task.kind === 'partition.maintain')).toBe(false)
    // The policy that exists says so, and its refusal raised one alert, not one per try.
    expect(await endpoints.getPartitionPolicy('shop', 'public', 'metrics')).toMatchObject({
      tableOwner: 'analyst',
      ownerTrusted: false,
    })
    await refused(endpoints.runPartitionPolicy('shop', 'public', 'metrics'))
    expect(
      getState().alerts.filter((a) => a.kind === 'partitionPolicyFailed' && a.state === 'open'),
    ).toHaveLength(1)
  })

  it('turning the setting on makes the owner trusted and lets the conversion and the policy through', async () => {
    sw('analytics=timeseries,owner:untrusted')
    await endpoints.updateClusterSettings({ analytics: { allowNonSuperuserOwners: true } })
    const pre = await endpoints.previewTableStorage('shop', 'public', 'clicks', {
      accessMethod: 'columnar',
    })
    expect(pre).toMatchObject({ ownerTrusted: true, willConvert: true, blockers: [] })
    expect(
      (await endpoints.putPartitionPolicy('shop', 'public', 'metrics', policy)).ownerTrusted,
    ).toBe(true)
    expect((await endpoints.runPartitionPolicy('shop', 'public', 'metrics')).kind).toBe(
      'partition.maintain',
    )
  })
})
