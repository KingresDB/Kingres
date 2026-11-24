// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The table lifecycle of contract 0.14.0 in the mock (increment 9.7): the preview of a change, the refusal of a table that is
// not eligible (422 `table_not_eligible` with the same lines), the tasks of the four kinds, the user partitions, and the cancel.
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
const reports = () => getState().databases.find((d) => d.info.name === 'reports')!
const table = (db: ReturnType<typeof shop>, name: string) => db.tables.find((t) => t.name === name)!
const done = (id: number) =>
  waitFor(async () => expect((await endpoints.getTask(id)).state).toBe('done'), {
    timeout: 30_000,
  })
const failedBlockers = async (promise: Promise<unknown>) => {
  const error = await promise.then(
    () => null,
    (e: unknown) => e,
  )
  expect(error).toBeInstanceOf(ApiError)
  const e = error as ApiError
  return { status: e.problem.status, code: e.code, blockers: e.problem.blockers ?? [] }
}

describe('the mock: preview and eligibility', () => {
  beforeEach(async () => {
    await signInAs(createPinia())
    mockConfig.taskDurationMs = 60
  })

  it('previewTableChange answers the contract for every kind of change', async () => {
    sw('tables=partitions')
    const asked = [
      [
        'notes',
        { kind: 'distribute', distribute: { method: 'hash', distributionColumn: 'note_id' } },
      ],
      ['customers', { kind: 'undistribute', undistribute: { cascadeViaForeignKeys: true } }],
      ['orders', { kind: 'alter', alter: { bucketCount: 256 } }],
      ['measurements', { kind: 'createPartition', partition: { name: 'p', bound: 'DEFAULT' } }],
      [
        'measurements',
        {
          kind: 'attachPartition',
          partition: { table: { schema: 'public', table: 'measurements_old' }, bound: 'DEFAULT' },
        },
      ],
      ['measurements', { kind: 'detachPartition', partition: { name: 'measurements_2026_01' } }],
      ['measurements', { kind: 'dropPartition', partition: { name: 'measurements_2026_01' } }],
    ] as const
    for (const [name, body] of asked) {
      const preview = await endpoints.previewTableChange('shop', 'public', name, body as never)
      ok(preview, 'TableChangePreview')
    }
  })

  it('a view that uses the table is a blocker, and a warning when it is to be made again', async () => {
    reports().info.sharding.enabled = true
    const ask = (views: 'refuse' | 'recreate') =>
      endpoints.previewTableChange('reports', 'public', 'monthly_totals', {
        kind: 'distribute',
        distribute: { method: 'hash', distributionColumn: 'month', dependentViews: views },
      })
    const refused = await ask('refuse')
    expect(refused.blockers.map((b) => b.code)).toEqual(['dependent_object'])
    expect(refused.blockers[0]?.object).toMatch(/monthly_totals_by_region/)
    const recreate = await ask('recreate')
    expect(recreate.blockers).toEqual([])
    expect(recreate.warnings.map((b) => b.code)).toEqual(['dependent_view'])
  })

  it('a table that is not eligible is refused with 422 table_not_eligible and the lines of the preview', async () => {
    reports().info.sharding.enabled = true
    const preview = await endpoints.previewTableChange('reports', 'public', 'exports', {
      kind: 'distribute',
      distribute: { method: 'hash', distributionColumn: 'export_id' },
    })
    expect(preview.blockers.map((b) => b.code)).toContain('trigger')
    const refusal = await failedBlockers(
      endpoints.distributeTable('reports', 'public', 'exports', {
        method: 'hash',
        distributionColumn: 'export_id',
      }),
    )
    expect(refusal).toMatchObject({ status: 422, code: 'table_not_eligible' })
    expect(refusal.blockers).toEqual(preview.blockers)
    expect(getState().tasks.some((t) => t.task.kind === 'table.distribute')).toBe(false)
  })

  it('the estimate recommends an online copy for a table with a key and many rows, and tells who waits', async () => {
    const preview = await endpoints.previewTableChange('shop', 'public', 'customers', {
      kind: 'undistribute',
      undistribute: {},
    })
    expect(preview.estimate).toMatchObject({
      modeRecommended: 'online',
      onlineAvailable: true,
      writersBlocked: 'switchOnly',
    })
    const blocking = await endpoints.previewTableChange('shop', 'public', 'customers', {
      kind: 'undistribute',
      undistribute: { mode: 'blocking' },
    })
    expect(blocking.estimate.writersBlocked).toBe('wholeCopy')
    const small = await endpoints.previewTableChange('shop', 'public', 'countries', {
      kind: 'undistribute',
      undistribute: {},
    })
    expect(small.estimate.modeRecommended).toBe('blocking')
  })

  it('distribute takes a bucket count of 1 to 4096, a mode and the views, and the new group gets that count', async () => {
    await expect(
      endpoints.distributeTable('shop', 'public', 'notes', {
        method: 'hash',
        distributionColumn: 'note_id',
        bucketCount: 5000,
      }),
    ).rejects.toMatchObject({ status: 400, code: 'validation_failed' })
    const task = await endpoints.distributeTable('shop', 'public', 'notes', {
      method: 'hash',
      distributionColumn: 'note_id',
      bucketCount: 64,
      mode: 'blocking',
    })
    expect(task.kind).toBe('table.distribute')
    expect(task.subject).toEqual({ type: 'table', id: 'shop/public.notes' })
    await done(task.id)
    expect(table(shop(), 'notes').distribution).toMatchObject({
      bucketCount: 64,
      column: 'note_id',
    })
    expect(shop().colocations.some((g) => g.bucketCount === 64)).toBe(true)
  })
})

describe('the mock: undistribute and alter', () => {
  beforeEach(async () => {
    await signInAs(createPinia())
    mockConfig.taskDurationMs = 60
  })

  it('a table linked by a foreign key is refused unless the linked tables go with it', async () => {
    const refused = await failedBlockers(
      endpoints.undistributeTable('shop', 'public', 'orders', { dependentViews: 'recreate' }),
    )
    expect(refused.code).toBe('table_not_eligible')
    expect(refused.blockers.map((b) => b.code)).toEqual(['foreign_key_linked'])
    expect(refused.blockers[0]?.object).toBe('public.customers')
    const preview = await endpoints.previewTableChange('shop', 'public', 'orders', {
      kind: 'undistribute',
      undistribute: { cascadeViaForeignKeys: true },
    })
    expect(preview.blockers).toEqual([])
    expect(preview.affectedTables.map((t) => [t.name, t.role])).toEqual([
      ['orders', 'source'],
      ['customers', 'linked'],
    ])
  })

  it('undistribute with the cascade makes both tables plain on the chosen group', async () => {
    const task = await endpoints.undistributeTable('shop', 'public', 'orders', {
      cascadeViaForeignKeys: true,
      targetGroupId: 2,
      dependentViews: 'recreate',
    })
    expect(task.kind).toBe('table.undistribute')
    await done(task.id)
    for (const name of ['orders', 'customers']) {
      expect(table(shop(), name)).toMatchObject({ distribution: null, homeShardGroupId: 2 })
    }
    expect(table(shop(), 'orders').classes).toEqual(['ledger'])
    expect(table(shop(), 'customers').classes).toEqual(['plain'])
    expect(shop().colocations.find((g) => g.colocationId === 1)?.tableCount).toBe(0)
  })

  it('a shard group that does not exist is refused with the line, a bad option with 400', async () => {
    const refused = await failedBlockers(
      endpoints.undistributeTable('shop', 'public', 'events', { targetGroupId: 99 }),
    )
    expect(refused.blockers.map((b) => b.code)).toContain('no_such_group')
    await expect(
      endpoints.undistributeTable('shop', 'public', 'events', { mode: 'sideways' as never }),
    ).rejects.toMatchObject({ status: 400 })
  })

  it('alter needs something to change, refuses online for a group change, and a column outside the unique key', async () => {
    await expect(
      endpoints.alterTableDistribution('shop', 'public', 'orders', {}),
    ).rejects.toMatchObject({ status: 400, code: 'validation_failed' })
    await expect(
      endpoints.alterTableDistribution('shop', 'public', 'orders', {
        bucketCount: 256,
        cascadeToColocated: true,
        mode: 'online',
      }),
    ).rejects.toMatchObject({ status: 400 })
    const refused = await failedBlockers(
      endpoints.alterTableDistribution('shop', 'public', 'orders', { distributionColumn: 'total' }),
    )
    expect(refused.blockers.map((b) => b.code)).toEqual(['not_in_unique_key'])
  })

  it('a bucket count makes a group of its own; a multiple of the old count is a note, not a copy', async () => {
    const preview = await endpoints.previewTableChange('shop', 'public', 'events', {
      kind: 'alter',
      alter: { bucketCount: 256 },
    })
    expect(preview.notes.map((n) => n.code)).toContain('multiple_buckets')
    expect(preview.estimate.writersBlocked).toBe('none')
    const task = await endpoints.alterTableDistribution('shop', 'public', 'events', {
      bucketCount: 256,
    })
    expect(task.progress.steps.map((s) => s.name)).toEqual(['Change the catalog'])
    await done(task.id)
    const events = table(shop(), 'events')
    expect(events.distribution?.bucketCount).toBe(256)
    expect(events.distribution?.colocationId).not.toBe(2)
    const copy = await endpoints.alterTableDistribution('shop', 'public', 'customers', {
      bucketCount: 100,
      dependentViews: 'recreate',
    })
    expect(copy.progress.steps.map((s) => s.name)).toContain('Copy rows')
  })

  it('a change of the tables of a colocation group together moves all of them', async () => {
    const task = await endpoints.alterTableDistribution('shop', 'public', 'orders', {
      bucketCount: 100,
      cascadeToColocated: true,
      mode: 'blocking',
      dependentViews: 'recreate',
    })
    await done(task.id)
    expect(table(shop(), 'orders').distribution?.colocationId).toBe(
      table(shop(), 'customers').distribution?.colocationId,
    )
    expect(table(shop(), 'orders').distribution?.bucketCount).toBe(100)
  })

  it('a user-partitioned table can neither be undistributed nor altered in this version', async () => {
    sw('tables=partitions')
    for (const refusal of [
      await failedBlockers(endpoints.undistributeTable('shop', 'public', 'measurements')),
      await failedBlockers(
        endpoints.alterTableDistribution('shop', 'public', 'measurements', { bucketCount: 64 }),
      ),
    ]) {
      expect(refusal.code).toBe('table_not_eligible')
      expect(refusal.blockers.map((b) => b.code)).toContain('user_partitioned')
    }
  })
})

describe('the mock: partitions', () => {
  beforeEach(async () => {
    await signInAs(createPinia())
    mockConfig.taskDurationMs = 60
    sw('tables=partitions')
  })

  it('lists the partitions as the contract says, and a table that is not partitioned has none', async () => {
    const page = await endpoints.listTablePartitions('shop', 'public', 'measurements')
    ok(page, 'TablePartitionPage')
    expect(page.items.map((p) => p.name)).toEqual([
      'measurements_2026_01',
      'measurements_2026_02',
      'measurements_default',
    ])
    expect(page.items[2]).toMatchObject({ isDefault: true, bound: 'DEFAULT' })
    expect((await endpoints.listTablePartitions('shop', 'public', 'orders')).items).toEqual([])
    const detail = await endpoints.getTable('shop', 'public', 'measurements')
    expect(detail.partitioning).toMatchObject({
      method: 'range',
      key: 'taken_at',
      partitionCount: 3,
    })
  })

  it('creates a partition (409 when it exists, unless ifNotExists), and the count follows', async () => {
    const task = await endpoints.createTablePartition('shop', 'public', 'measurements', {
      name: 'measurements_2026_03',
      bound: "FROM ('2026-03-01') TO ('2026-04-01')",
    })
    expect(task.kind).toBe('table.partition')
    await done(task.id)
    expect((await endpoints.getTable('shop', 'public', 'measurements')).partitioning).toMatchObject(
      {
        partitionCount: 4,
      },
    )
    await expect(
      endpoints.createTablePartition('shop', 'public', 'measurements', {
        name: 'measurements_2026_03',
        bound: 'DEFAULT',
      }),
    ).rejects.toMatchObject({ status: 409, code: 'already_exists' })
    const again = await endpoints.createTablePartition('shop', 'public', 'measurements', {
      name: 'measurements_2026_03',
      bound: 'DEFAULT',
      ifNotExists: true,
    })
    await done(again.id)
    expect(
      (await endpoints.listTablePartitions('shop', 'public', 'measurements')).items,
    ).toHaveLength(4)
  })

  it('a name of more than 57 bytes and an empty bound are 400, a bound that is not one is refused with its line', async () => {
    await expect(
      endpoints.createTablePartition('shop', 'public', 'measurements', {
        name: 'x'.repeat(58),
        bound: 'DEFAULT',
      }),
    ).rejects.toMatchObject({ status: 400 })
    await expect(
      endpoints.createTablePartition('shop', 'public', 'measurements', { name: 'p', bound: ' ' }),
    ).rejects.toMatchObject({ status: 400 })
    const refused = await failedBlockers(
      endpoints.createTablePartition('shop', 'public', 'measurements', {
        name: 'p',
        bound: 'banana',
      }),
    )
    expect(refused.blockers.map((b) => b.code)).toEqual(['invalid_bound'])
  })

  it('drops a partition (404 when it does not exist) and detaches one as a distributed table of its own', async () => {
    await expect(
      endpoints.dropTablePartition('shop', 'public', 'measurements', 'nope'),
    ).rejects.toMatchObject({ status: 404 })
    const drop = await endpoints.dropTablePartition(
      'shop',
      'public',
      'measurements',
      'measurements_default',
    )
    await done(drop.id)
    const detach = await endpoints.detachTablePartition(
      'shop',
      'public',
      'measurements',
      'measurements_2026_01',
    )
    await done(detach.id)
    const left = await endpoints.listTablePartitions('shop', 'public', 'measurements')
    expect(left.items.map((p) => p.name)).toEqual(['measurements_2026_02'])
    expect(table(shop(), 'measurements_2026_01')).toMatchObject({
      classes: ['distributed'],
      distribution: { column: 'sensor_id' },
    })
  })

  it('attaches a table of the same colocation group and column; another table is refused', async () => {
    const wrong = await failedBlockers(
      endpoints.attachTablePartition('shop', 'public', 'measurements', {
        table: { schema: 'public', table: 'customers' },
        bound: 'DEFAULT',
      }),
    )
    expect(wrong.blockers.map((b) => b.code)).toEqual(['not_in_colocation_group'])
    const task = await endpoints.attachTablePartition('shop', 'public', 'measurements', {
      table: { schema: 'public', table: 'measurements_old' },
      bound: "FROM ('2025-01-01') TO ('2026-01-01')",
    })
    await done(task.id)
    expect(shop().tables.some((t) => t.name === 'measurements_old')).toBe(false)
    expect(
      (await endpoints.listTablePartitions('shop', 'public', 'measurements')).items.map(
        (p) => p.name,
      ),
    ).toContain('measurements_old')
  })

  it('a table with foreign keys cannot detach a partition', async () => {
    sw('tables=fk:measurements')
    const refused = await failedBlockers(
      endpoints.detachTablePartition('shop', 'public', 'measurements', 'measurements_2026_01'),
    )
    expect(refused.blockers.map((b) => b.code)).toEqual(['foreign_key'])
  })

  it('a table that is not partitioned answers the change of partitions with its line', async () => {
    const refused = await failedBlockers(
      endpoints.createTablePartition('shop', 'public', 'customers', {
        name: 'p',
        bound: 'DEFAULT',
      }),
    )
    expect(refused.blockers.map((b) => b.code)).toEqual(['not_partitioned'])
  })
})

describe('the mock: tasks that run, fail and are cancelled', () => {
  beforeEach(async () => {
    await signInAs(createPinia())
    mockConfig.taskDurationMs = 60
  })

  it('refuses a change of a table, or of its colocation group, while another change holds it (409)', async () => {
    sw('tables=slow')
    const first = await endpoints.alterTableDistribution('shop', 'public', 'orders', {
      bucketCount: 100,
      dependentViews: 'recreate',
    })
    expect(first.state).toBe('running')
    await expect(
      endpoints.alterTableDistribution('shop', 'public', 'orders', { bucketCount: 90 }),
    ).rejects.toMatchObject({ status: 409, code: 'operation_in_progress' })
    await expect(
      endpoints.undistributeTable('shop', 'public', 'customers', { dependentViews: 'recreate' }),
    ).rejects.toMatchObject({ status: 409, code: 'operation_in_progress' })
    // A table of another colocation group is free.
    const other = await endpoints.undistributeTable('shop', 'public', 'events')
    expect(other.state).toBe('running')
  })

  it('a busy colocation group (another operation) refuses a change and lists group_busy in the preview', async () => {
    sw('tables=busy')
    const preview = await endpoints.previewTableChange('shop', 'public', 'orders', {
      kind: 'alter',
      alter: { bucketCount: 100 },
    })
    expect(preview.blockers.map((b) => b.code)).toContain('group_busy')
    await expect(
      endpoints.alterTableDistribution('shop', 'public', 'orders', { bucketCount: 100 }),
    ).rejects.toMatchObject({ status: 409, code: 'operation_in_progress' })
  })

  it('fail:space ends the task at its first step with the reason, and the table is as it was', async () => {
    sw('tables=fail:space')
    const task = await endpoints.undistributeTable('shop', 'public', 'events')
    await waitFor(async () => expect((await endpoints.getTask(task.id)).state).toBe('failed'), {
      timeout: 30_000,
    })
    const failed = await endpoints.getTask(task.id)
    expect(failed.error).toMatchObject({ code: 'insufficient_space', step: 'Check free space' })
    expect(failed.error?.message).toMatch(/free space/)
    expect(table(shop(), 'events').distribution).not.toBeNull()
  })

  it('fail:copy ends the task in the copy', async () => {
    sw('tables=fail:copy')
    const task = await endpoints.undistributeTable('shop', 'public', 'events')
    await waitFor(async () => expect((await endpoints.getTask(task.id)).state).toBe('failed'), {
      timeout: 30_000,
    })
    expect((await endpoints.getTask(task.id)).error).toMatchObject({
      code: 'copy_failed',
      step: 'Copy rows',
    })
  })

  it('a task is cancelled until its switch: it ends failed with `cancelled` and the table is as it was', async () => {
    sw('tables=slow')
    const task = await endpoints.undistributeTable('shop', 'public', 'events')
    expect(task.cancellable).toBe(true)
    const cancelled = await endpoints.cancelTask(task.id)
    expect(cancelled).toMatchObject({ state: 'failed', error: { code: 'cancelled' } })
    expect(table(shop(), 'events').distribution).not.toBeNull()
    // The task is over: a second cancel is refused.
    await expect(endpoints.cancelTask(task.id)).rejects.toMatchObject({ status: 409 })
  })

  it('from the switch on the task goes on to its end and cannot be cancelled', async () => {
    sw('tables=slow')
    const task = await endpoints.undistributeTable('shop', 'public', 'events')
    const rec = getState().tasks.find((t) => t.task.id === task.id)!
    rec.startedAt = Date.now() - 0.8 * rec.durationMs
    const later = await endpoints.getTask(task.id)
    expect(later.cancellable).toBe(false)
    await expect(endpoints.cancelTask(task.id)).rejects.toMatchObject({
      code: 'task_not_cancellable',
    })
  })
})
