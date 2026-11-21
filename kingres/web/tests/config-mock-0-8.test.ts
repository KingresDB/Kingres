// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.8.0, the mock API of the tag Configuration: the catalogue and its policy, the values on every server,
// preview and change with the checks the real server makes, the task with a step per server (failed, waiting, retry),
// history and revert, restart and its order, the clocks, the summary, and who may do what. The panel is built against
// this mock, so what it does is part of what the panel is tested against.
import { describe, expect, it } from 'vitest'
import { endpoints } from '@/api/endpoints'
import { ApiError } from '@/api/problem'
import { API_BASE_PATH, setCsrfToken } from '@/api/client'
import { getState } from '@/mocks/http'
import {
  bringConfigServerBack,
  failNextApply,
  seedRestartConflict,
  setClockOffset,
  setClockReachable,
  takeConfigServerDown,
} from '@/mocks/config'
import type { ConfigChangeItem, ConfigParameter, Task } from '@/api/types'

async function login(user = 'admin'): Promise<string> {
  const session = await endpoints.login({ username: user, password: 'demo-password' })
  const token = session.csrfToken ?? null
  setCsrfToken(token)
  return token ?? ''
}

async function all(query: Record<string, unknown> = {}): Promise<ConfigParameter[]> {
  const out: ConfigParameter[] = []
  let cursor: string | undefined
  for (let i = 0; i < 20; i += 1) {
    const page = await endpoints.listConfigParameters({ limit: 500, cursor, ...query })
    out.push(...page.items)
    if (!page.nextCursor) break
    cursor = page.nextCursor
  }
  return out
}

async function settle(task: Task): Promise<Task> {
  for (let i = 0; i < 200; i += 1) {
    const t = await endpoints.getTask(task.id)
    if (t.state !== 'running') return t
    await new Promise((r) => setTimeout(r, 15))
  }
  throw new Error('the task did not settle')
}

const change = (
  name: string,
  value: string | null,
  level: ConfigChangeItem['level'] = 'cluster',
  scopeId?: string,
): ConfigChangeItem => ({ name, level, value, ...(scopeId ? { scopeId } : {}) })

async function problemOf(promise: Promise<unknown>): Promise<ApiError> {
  try {
    await promise
  } catch (e) {
    if (e instanceof ApiError) return e
    throw e
  }
  throw new Error('the request was not refused')
}

const param = async (name: string): Promise<ConfigParameter> => endpoints.getConfigParameter(name)

describe('the catalogue', () => {
  it('lists some three hundred parameters of every type, with the extensions, ordered by name', async () => {
    await login()
    const items = await all({})
    expect(items.length).toBeGreaterThan(300)
    expect(new Set(items.map((p) => p.type))).toEqual(
      new Set(['bool', 'integer', 'real', 'string', 'enum']),
    )
    const names = items.map((p) => p.name.toLowerCase())
    expect(names).toEqual([...names].sort())
    expect(items.some((p) => p.name === 'kingres_shard.node_timeout')).toBe(true)
    expect(items.some((p) => p.name === 'kingres_ledger.block_interval')).toBe(true)
    expect(new Set(items.map((p) => p.category)).has('Resource Usage / Memory')).toBe(true)
    expect(items.some((p) => p.context === 'postmaster')).toBe(true)
    // Units, ranges and enum values come from the catalogue.
    const wm = items.find((p) => p.name === 'work_mem')!
    expect(wm).toMatchObject({
      unit: 'kB',
      min: '64',
      max: '2147483647',
      defaultValue: '4MB',
      context: 'user',
    })
    expect(items.find((p) => p.name === 'wal_level')!.enumValues).toEqual([
      'minimal',
      'replica',
      'logical',
    ])
    expect(items.find((p) => p.name === 'shared_buffers')).toMatchObject({
      restartRequired: true,
      unit: '8kB',
    })
  })

  it('pages by cursor and filters by text, category, kind and state', async () => {
    await login()
    const page = await endpoints.listConfigParameters({ limit: 50 })
    expect(page.items).toHaveLength(50)
    expect(page.nextCursor).not.toBeNull()
    expect(page.revision).toBe(6)
    const next = await endpoints.listConfigParameters({ limit: 50, cursor: page.nextCursor! })
    expect(next.items[0]!.name).not.toBe(page.items[0]!.name)

    const text = await all({ q: 'WORK_MEM' })
    expect(text.map((p) => p.name)).toContain('work_mem')
    expect(text.map((p) => p.name)).toContain('maintenance_work_mem')
    const byDescription = await all({ q: 'two-phase' })
    expect(byDescription.map((p) => p.name)).not.toContain('work_mem')

    const memory = await all({ category: 'Resource Usage / Memory' })
    expect(memory.length).toBeGreaterThan(5)
    expect(memory.every((p) => p.category === 'Resource Usage / Memory')).toBe(true)
    // A top-level category matches its sub-categories.
    const top = await all({ category: 'Resource Usage' })
    expect(top.length).toBeGreaterThan(memory.length)

    expect((await all({ kind: 'managed' })).every((p) => p.kind === 'managed')).toBe(true)
    expect((await all({ kind: 'refused' })).every((p) => p.kind === 'refused')).toBe(true)
    const settable = await all({ kind: 'settable' })
    expect(settable.every((p) => p.kind === 'settable')).toBe(true)
    expect((await all({ changed: true })).map((p) => p.name).sort()).toEqual(
      ['TimeZone', 'effective_io_concurrency', 'shared_buffers', 'work_mem'].sort(),
    )
    expect((await all({ pendingRestart: true })).map((p) => p.name)).toEqual(['shared_buffers'])
    expect((await all({ state: 'pendingRestart' })).map((p) => p.name)).toEqual(['shared_buffers'])
    expect((await all({ state: 'failed' })).length).toBe(0)
  })

  it('knows a parameter in any letter case and says when there is none', async () => {
    await login()
    expect((await param('TIMEZONE')).name).toBe('TimeZone')
    expect((await param('WORK_MEM')).name).toBe('work_mem')
    const e = await problemOf(endpoints.getConfigParameter('no_such_setting'))
    expect(e.code).toBe('config_parameter_unknown')
    expect(e.status).toBe(404)
  })
})

describe('policy: managed, refused, hidden and cluster-only parameters', () => {
  it('lists managed parameters read-only with the reason, and their values', async () => {
    await login()
    const port = await param('port')
    expect(port).toMatchObject({ kind: 'managed', allowedLevels: [], hidden: false })
    expect(port.reason).toMatch(/port/i)
    expect(port.servers.map((s) => s.value)).toEqual(['5432', '5432', '5432'])
    expect(port.servers[0]!.source).toBe('managed')
    for (const name of [
      'listen_addresses',
      'ssl',
      'synchronous_standby_names',
      'wal_level',
      'cluster_name',
    ])
      expect((await param(name)).kind, name).toBe('managed')
    expect((await param('shared_preload_libraries')).kind).toBe('managed')
  })

  it('lists refused parameters read-only, and never returns the value of a hidden one', async () => {
    await login()
    for (const name of [
      'archive_command',
      'restore_command',
      'ssl_passphrase_command',
      'dynamic_library_path',
      'session_preload_libraries',
      'fsync',
      'full_page_writes',
      'allow_system_table_mods',
    ]) {
      const p = await param(name)
      expect(p.kind, name).toBe('refused')
      expect(p.allowedLevels, name).toEqual([])
      expect(p.reason, name).toBeTruthy()
    }
    const hidden = await param('archive_command')
    expect(hidden.hidden).toBe(true)
    expect(hidden.defaultValue).toBeNull()
    expect(hidden.servers.every((s) => s.value === null && s.pendingValue === null)).toBe(true)
    expect(hidden.overrides).toEqual([])
    // A refused parameter that is not secret still shows its value.
    const fsync = await param('fsync')
    expect(fsync.hidden).toBe(false)
    expect(fsync.servers[0]!.value).toBe('on')
    const hiddenAll = (await all({})).filter((p) => p.hidden)
    expect(hiddenAll.length).toBeGreaterThan(15)
    expect(hiddenAll.every((p) => p.servers.every((s) => s.value === null))).toBe(true)
  })

  it('marks the parameters that must be equal on every server and offers them no server level', async () => {
    await login()
    const tz = await param('TimeZone')
    expect(tz.allowedLevels).toEqual(['cluster', 'database'])
    expect(tz.clusterOnly).toMatch(/another node|differs/i)
    expect((await param('log_timezone')).allowedLevels).toEqual(['cluster'])
    for (const name of [
      'DateStyle',
      'lc_numeric',
      'kingres_shard.default_bucket_count',
      'kingres_ledger.block_interval',
      'max_prepared_transactions',
    ])
      expect((await param(name)).clusterOnly, name).not.toBeNull()
    // Settings that only tune one server have every level their context allows.
    expect((await param('work_mem')).allowedLevels).toEqual([
      'cluster',
      'serverGroup',
      'server',
      'database',
    ])
    expect((await param('shared_buffers')).allowedLevels).toEqual([
      'cluster',
      'serverGroup',
      'server',
    ])
    expect((await param('kingres_shard.node_timeout')).clusterOnly).toBeNull()
  })
})

describe('the values on the servers', () => {
  it('shows shared_buffers pending a restart on one server only', async () => {
    await login()
    const p = await param('shared_buffers')
    const by = Object.fromEntries(p.servers.map((s) => [s.serverId, s]))
    expect(by['10.0.0.11']).toMatchObject({
      value: '256MB',
      state: 'applied',
      level: 'cluster',
      source: 'cluster',
    })
    expect(by['10.0.0.12']).toMatchObject({
      value: '128MB',
      state: 'pendingRestart',
      pendingValue: '256MB',
      level: 'cluster',
    })
    expect(by['10.0.0.13']!.state).toBe('applied')
    expect(p.differs).toBe(true)
    // Two servers of the same group differ and no server-level override explains it.
    expect(p.differsInGroup).toBe(true)
  })

  it('shows work_mem overridden on one server so that it differs, and explained', async () => {
    await login()
    const p = await param('work_mem')
    expect(p.servers.map((s) => s.value)).toEqual(['8MB', '8MB', '32MB'])
    expect(p.servers[2]).toMatchObject({ source: 'server', level: 'server', state: 'applied' })
    expect(p.differs).toBe(true)
    expect(p.differsInGroup).toBe(false)
    expect(p.overrides.map((o) => [o.level, o.scopeId, o.value])).toEqual(
      expect.arrayContaining([
        ['cluster', null, '8MB'],
        ['server', '10.0.0.13', '32MB'],
      ]),
    )
    expect((await param('effective_io_concurrency')).servers.map((s) => s.value)).toEqual([
      '200',
      '200',
      '16',
    ])
  })

  it('shows where a value comes from and a value that nothing explains', async () => {
    await login()
    expect((await param('TimeZone')).servers.map((s) => [s.value, s.source])).toEqual([
      ['Asia/Tehran', 'initdb'],
      ['Asia/Tehran', 'initdb'],
      ['Asia/Tehran', 'initdb'],
    ])
    expect((await param('max_connections')).servers[0]!.source).toBe('derived')
    expect((await param('jit')).servers[0]!.source).toBe('default')
    const cp = await param('checkpoint_timeout')
    expect(cp.servers.map((s) => s.value)).toEqual(['5min', '10min', '5min'])
    expect(cp.servers[1]!.source).toBe('other')
    expect(cp.differsInGroup).toBe(true)
  })

  it('answers as partial and names a server that could not be read', async () => {
    await login()
    const { mockConfig } = await import('@/mocks/state')
    mockConfig.partial = [{ serverId: '10.0.0.12', reason: 'timeout' }]
    const page = await endpoints.listConfigParameters({ q: 'work_mem' })
    expect(page.partial).toBe(true)
    expect(page.missingServers).toEqual([{ serverId: '10.0.0.12', reason: 'timeout' }])
    const wm = page.items.find((p) => p.name === 'work_mem')!
    expect(wm.servers[1]).toMatchObject({ state: 'unknown', value: null })
    mockConfig.partial = null
  })
})

describe('preview', () => {
  it('says which servers change and how, and that nothing is restarted', async () => {
    await login()
    const r = await endpoints.previewConfigChange({ changes: [change('work_mem', '16MB')] })
    expect(r.valid).toBe(true)
    const by = Object.fromEntries(r.effects.map((e) => [e.serverId, e]))
    expect(by['10.0.0.11']).toMatchObject({ before: '8MB', after: '16MB', takesEffect: 'reload' })
    expect(by['10.0.0.12']!.takesEffect).toBe('reload')
    // The server override beats the cluster value, so this server does not change.
    expect(by['10.0.0.13']).toMatchObject({
      before: '32MB',
      after: '32MB',
      takesEffect: 'unchanged',
    })
    expect(r.restartServerIds).toEqual([])
    expect(r.restartOrderConflict).toBeNull()
    expect(getState().config.revision).toBe(6)
  })

  it('needs a restart for a postmaster setting and orders the instances', async () => {
    await login()
    const up = await endpoints.previewConfigChange({ changes: [change('max_connections', '200')] })
    expect(up.valid).toBe(true)
    expect(up.effects.every((e) => e.takesEffect === 'restart')).toBe(true)
    expect(up.restartServerIds.sort()).toEqual(['10.0.0.11', '10.0.0.12', '10.0.0.13'])
    // A value that goes up: every standby before its primary.
    const roles = up.restartOrder.map((s) => s.role)
    expect(roles.indexOf('primary')).toBeGreaterThan(roles.lastIndexOf('standby'))
    expect(roles.filter((r) => r === 'standby').length).toBeGreaterThan(0)

    const down = await endpoints.previewConfigChange({ changes: [change('max_connections', '50')] })
    const downRoles = down.restartOrder.map((s) => s.role)
    // A value that goes down: every primary before its standby.
    expect(downRoles.lastIndexOf('primary')).toBeLessThan(downRoles.indexOf('standby'))

    // Not one of the five settings: no direction, no conflict.
    const other = await endpoints.previewConfigChange({
      changes: [change('shared_buffers', '512MB')],
    })
    expect(other.restartOrderConflict).toBeNull()
  })

  it('refuses a mixed change with the settings named', async () => {
    await login()
    const r = await endpoints.previewConfigChange({
      changes: [change('max_connections', '200'), change('max_locks_per_transaction', '16')],
    })
    expect(r.valid).toBe(true)
    expect(r.restartOrderConflict).toMatch(/max_connections.*up.*max_locks_per_transaction.*down/)
    expect(r.restartOrder).toEqual([])
  })

  it('reports a server that is down as waiting', async () => {
    await login()
    takeConfigServerDown(getState(), '10.0.0.12')
    const r = await endpoints.previewConfigChange({ changes: [change('work_mem', '16MB')] })
    expect(r.effects.find((e) => e.serverId === '10.0.0.12')!.takesEffect).toBe('waits')
  })

  it('is a question: it answers 200 with the problems of a change that would be refused', async () => {
    await login()
    const cases: [ConfigChangeItem, string, string][] = [
      [change('no_such', '1'), 'config_parameter_unknown', '/changes/0/name'],
      [change('port', '5433'), 'config_parameter_managed', '/changes/0/name'],
      [change('archive_command', 'cp %p /x'), 'config_parameter_refused', '/changes/0/name'],
      [change('fsync', 'off'), 'config_parameter_refused', '/changes/0/name'],
      [
        change('TimeZone', 'Asia/Tehran', 'server', '10.0.0.11'),
        'config_level_not_allowed',
        '/changes/0/level',
      ],
      [
        change('shared_buffers', '1GB', 'database', 'shop'),
        'config_level_not_allowed',
        '/changes/0/level',
      ],
      [change('work_mem', '63kB'), 'config_value_invalid', '/changes/0/value'],
      [change('work_mem', '0x40kB'), 'config_value_invalid', '/changes/0/value'],
      [change('work_mem', '8MB', 'server', 'nope'), 'not_found', '/changes/0/scopeId'],
      [change('work_mem', '8MB', 'database', 'nope'), 'not_found', '/changes/0/scopeId'],
      [change('work_mem', '8MB', 'serverGroup', 'nope'), 'not_found', '/changes/0/scopeId'],
      [change('work_mem', '8MB', 'server'), 'not_found', '/changes/0/scopeId'],
      [change('TimeZone', 'Mars/Olympus'), 'config_value_invalid', '/changes/0/value'],
      [change('max_prepared_transactions', '0'), 'config_value_below_floor', '/changes/0/value'],
      [change('max_wal_senders', '2'), 'config_value_below_floor', '/changes/0/value'],
      [change('max_replication_slots', '1'), 'config_value_below_floor', '/changes/0/value'],
    ]
    for (const [item, code, pointer] of cases) {
      const r = await endpoints.previewConfigChange({ changes: [item] })
      expect(r.valid, `${item.name} ${item.value}`).toBe(false)
      expect(r.problems[0], `${item.name} ${item.value}`).toMatchObject({ code, pointer, index: 0 })
      expect(r.problems[0]!.message.length).toBeGreaterThan(10)
      expect(r.effects).toEqual([])
    }
  })

  it('names the change that is wrong when there are several', async () => {
    await login()
    const r = await endpoints.previewConfigChange({
      changes: [change('work_mem', '16MB'), change('work_mem', 'lots', 'server', '10.0.0.11')],
    })
    expect(r.valid).toBe(false)
    expect(r.problems).toHaveLength(1)
    expect(r.problems[0]).toMatchObject({ index: 1, pointer: '/changes/1/value' })
  })

  it('applies the floor with the replica count of the cluster', async () => {
    await login()
    getState().settings.defaultReplicaCount = 2
    const r = await endpoints.previewConfigChange({ changes: [change('max_wal_senders', '5')] })
    expect(r.problems[0]).toMatchObject({ code: 'config_value_below_floor' })
    expect(r.problems[0]!.message).toMatch(/at least 6/)
  })
})

describe('applying a change', () => {
  it('answers 202 with the task and its Location, and records the revision', async () => {
    const token = await login()
    const res = await fetch(`${API_BASE_PATH}/config/changes`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': token },
      body: JSON.stringify({ changes: [change('work_mem', '16MB')] }),
    })
    expect(res.status).toBe(202)
    const task = (await res.json()) as Task
    expect(res.headers.get('Location')).toBe(`${API_BASE_PATH}/tasks/${task.id}`)
    expect(task.kind).toBe('config.apply')
    expect(task.subject).toEqual({ type: 'config', id: 'cluster' })
    expect(task.progress.steps.map((s) => s.name)).toEqual([
      'Validate the change',
      'Record revision 7',
      'Apply on server 10.0.0.11',
      'Apply on server 10.0.0.12',
      'Apply on server 10.0.0.13',
      'Confirm',
    ])
    expect((await settle(task)).state).toBe('done')
    const p = await param('work_mem')
    expect(p.servers.map((s) => s.value)).toEqual(['16MB', '16MB', '32MB'])
    expect(p.servers.every((s) => s.state === 'applied')).toBe(true)
    expect((await endpoints.listConfigServers()).revision).toBe(7)
  })

  it('refuses before anything is written: 400 for a value, 422 for the policy, 404 for a scope', async () => {
    await login()
    const bad = await problemOf(
      endpoints.applyConfigChange({ changes: [change('work_mem', '63kB')] }),
    )
    expect(bad.code).toBe('validation_failed')
    expect(bad.status).toBe(400)
    expect(bad.problem.errors?.[0]).toMatchObject({ field: '/changes/0/value' })
    expect(
      (await problemOf(endpoints.applyConfigChange({ changes: [change('port', '1')] }))).code,
    ).toBe('config_parameter_managed')
    expect(
      (await problemOf(endpoints.applyConfigChange({ changes: [change('archive_command', 'x')] })))
        .code,
    ).toBe('config_parameter_refused')
    expect(
      (
        await problemOf(
          endpoints.applyConfigChange({
            changes: [change('TimeZone', 'UTC', 'server', '10.0.0.11')],
          }),
        )
      ).code,
    ).toBe('config_level_not_allowed')
    expect(
      (
        await problemOf(
          endpoints.applyConfigChange({ changes: [change('max_prepared_transactions', '0')] }),
        )
      ).code,
    ).toBe('config_value_below_floor')
    const gone = await problemOf(
      endpoints.applyConfigChange({ changes: [change('work_mem', '8MB', 'database', 'nope')] }),
    )
    expect(gone.status).toBe(404)
    // A valid change next to an invalid one: nothing is written.
    await problemOf(
      endpoints.applyConfigChange({
        changes: [change('work_mem', '16MB'), change('jit', 'maybe')],
      }),
    )
    expect(getState().config.revision).toBe(6)
    expect(getState().tasks.some((t) => t.task.kind === 'config.apply')).toBe(false)
  })

  it('waits for a restart where a setting needs one, and restarts nothing', async () => {
    await login()
    const task = await endpoints.applyConfigChange({ changes: [change('max_connections', '200')] })
    expect((await settle(task)).state).toBe('done')
    const p = await param('max_connections')
    expect(p.servers.map((s) => [s.value, s.state, s.pendingValue])).toEqual([
      ['100', 'pendingRestart', '200'],
      ['100', 'pendingRestart', '200'],
      ['100', 'pendingRestart', '200'],
    ])
    const servers = await endpoints.listConfigServers()
    expect(servers.items.every((s) => s.pendingRestart.includes('max_connections'))).toBe(true)
    expect(servers.items.every((s) => s.state === 'pendingRestart')).toBe(true)
  })

  it('applies at the server group, server and database level', async () => {
    await login()
    await settle(
      await endpoints.applyConfigChange({
        changes: [change('work_mem', '12MB', 'serverGroup', 'rack-b')],
      }),
    )
    expect((await param('work_mem')).servers[2]).toMatchObject({ value: '32MB', level: 'server' })
    await settle(
      await endpoints.applyConfigChange({
        changes: [change('work_mem', null, 'server', '10.0.0.13')],
      }),
    )
    const p = await param('work_mem')
    expect(p.servers.map((s) => [s.value, s.source])).toEqual([
      ['8MB', 'cluster'],
      ['8MB', 'cluster'],
      ['12MB', 'serverGroup'],
    ])
    await settle(
      await endpoints.applyConfigChange({
        changes: [change('TimeZone', 'Europe/Paris', 'database', 'shop')],
      }),
    )
    const time = await endpoints.getClusterTime()
    expect(time.databases).toEqual([
      { name: 'shop', timeZone: 'Europe/Paris' },
      { name: 'reports', timeZone: 'Europe/Berlin' },
    ])
  })

  it('removes an override: the lower level shows again', async () => {
    await login()
    await settle(await endpoints.applyConfigChange({ changes: [change('work_mem', null)] }))
    const p = await param('work_mem')
    expect(p.servers.map((s) => [s.value, s.source])).toEqual([
      ['4MB', 'default'],
      ['4MB', 'default'],
      ['32MB', 'server'],
    ])
  })

  it('sets the time zone and log_timezone together', async () => {
    await login()
    await settle(
      await endpoints.applyConfigChange({
        changes: [change('TimeZone', 'asia/kolkata'), change('log_timezone', 'Asia/Kolkata')],
      }),
    )
    for (const name of ['TimeZone', 'log_timezone']) {
      const p = await param(name)
      expect(
        p.servers.map((s) => [s.value, s.source]),
        name,
      ).toEqual([
        ['Asia/Kolkata', 'cluster'],
        ['Asia/Kolkata', 'cluster'],
        ['Asia/Kolkata', 'cluster'],
      ])
    }
  })

  it('is one configuration task at a time (409 operation_in_progress)', async () => {
    await login()
    const first = await endpoints.applyConfigChange({ changes: [change('work_mem', '16MB')] })
    const second = await problemOf(
      endpoints.applyConfigChange({ changes: [change('work_mem', '20MB')] }),
    )
    expect(second.code).toBe('operation_in_progress')
    expect(second.status).toBe(409)
    expect(
      (await problemOf(endpoints.restartPendingInstances({ acknowledgeDowntime: true }))).code,
    ).toBe('operation_in_progress')
    await settle(first)
    await settle(await endpoints.applyConfigChange({ changes: [change('work_mem', '20MB')] }))
  })

  it('does not record a change that changes nothing, and still runs the servers', async () => {
    await login()
    const task = await endpoints.applyConfigChange({ changes: [change('work_mem', '8MB')] })
    expect((await settle(task)).state).toBe('done')
    expect(getState().config.revision).toBe(6)
  })

  it('is audited', async () => {
    await login()
    await settle(await endpoints.applyConfigChange({ changes: [change('work_mem', '16MB')] }))
    expect(
      getState().audit.some((e) => e.action === 'applyConfigChange' && e.outcome === 'success'),
    ).toBe(true)
  })
})

describe('a server that fails or cannot be reached', () => {
  it('lets the others finish, ends the task failed and names the server and the text', async () => {
    await login()
    failNextApply(getState(), '10.0.0.12', 'invalid value for parameter "work_mem": "9000000000"')
    const task = await settle(
      await endpoints.applyConfigChange({ changes: [change('work_mem', '16MB')] }),
    )
    expect(task.state).toBe('failed')
    expect(task.error).toMatchObject({ code: 'config_incomplete', retryable: true })
    expect(task.error!.message).toMatch(/failed on 10\.0\.0\.12/)
    expect(task.progress.steps.find((s) => s.name === 'Apply on server 10.0.0.12')).toMatchObject({
      state: 'failed',
      detail: expect.stringContaining('invalid value'),
    })
    const p = await param('work_mem')
    expect(p.servers.map((s) => [s.value, s.state])).toEqual([
      ['16MB', 'applied'],
      ['8MB', 'failed'],
      ['32MB', 'applied'],
    ])
    expect(p.servers[1]!.error).toMatch(/invalid value/)
    const list = await endpoints.listConfigServers()
    expect(list.items[1]).toMatchObject({
      state: 'failed',
      failed: [{ name: 'work_mem', error: expect.stringContaining('invalid value') }],
    })
    expect(list.items[0]!.state).toBe('applied')
    // The desired state is recorded: the revision moved.
    expect(list.revision).toBe(7)
  })

  it('applies the same change again on a retry', async () => {
    await login()
    failNextApply(getState(), '10.0.0.12')
    const body = { changes: [change('work_mem', '16MB')] }
    expect((await settle(await endpoints.applyConfigChange(body))).state).toBe('failed')
    const retry = await settle(await endpoints.applyConfigChange(body))
    expect(retry.state).toBe('done')
    const p = await param('work_mem')
    expect(p.servers.map((s) => [s.value, s.state])).toEqual([
      ['16MB', 'applied'],
      ['16MB', 'applied'],
      ['32MB', 'applied'],
    ])
    // The retry is not a second change.
    expect(getState().config.revision).toBe(7)
  })

  it('waits for a server that cannot be reached and never gives up, then finishes when it is back', async () => {
    await login()
    takeConfigServerDown(getState(), '10.0.0.13')
    const task = await endpoints.applyConfigChange({
      changes: [change('work_mem', '16MB', 'server', '10.0.0.13')],
    })
    await new Promise((r) => setTimeout(r, 200))
    const running = await endpoints.getTask(task.id)
    expect(running.state).toBe('running')
    expect(running.detail).toMatch(/Waiting for server 10\.0\.0\.13, which cannot be reached/)
    expect(running.cancellable).toBe(true)
    const list = await endpoints.listConfigServers()
    expect(list.items[2]).toMatchObject({
      state: 'waiting',
      detail: expect.stringMatching(/not running/),
    })
    expect(list.items[0]!.state).toBe('applied')
    expect((await param('work_mem')).servers[2]).toMatchObject({ state: 'waiting', value: '32MB' })
    bringConfigServerBack(getState(), '10.0.0.13')
    expect((await settle(task)).state).toBe('done')
    expect((await param('work_mem')).servers[2]).toMatchObject({ state: 'applied', value: '16MB' })
  })

  it('leaves the server waiting when the task is cancelled', async () => {
    await login()
    takeConfigServerDown(getState(), '10.0.0.13')
    const task = await endpoints.applyConfigChange({
      changes: [change('work_mem', '16MB', 'server', '10.0.0.13')],
    })
    await endpoints.cancelTask(task.id)
    const list = await endpoints.listConfigServers()
    expect(list.items[2]!.state).toBe('waiting')
  })
})

describe('history and revert', () => {
  it('lists the changes newest first and marks only the latest of a level and scope revertible', async () => {
    await login()
    const list = await endpoints.listConfigChanges({ limit: 50 })
    expect(list.items.map((c) => c.changeId)).toEqual([6, 5, 4, 3, 2, 1])
    const byId = Object.fromEntries(list.items.map((c) => [c.changeId, c]))
    // work_mem at the cluster level was changed twice: only the later one can be reverted.
    expect(byId[6]).toMatchObject({
      name: 'work_mem',
      oldValue: '16MB',
      newValue: '8MB',
      revertible: true,
    })
    expect(byId[2]).toMatchObject({
      name: 'work_mem',
      oldValue: null,
      newValue: '16MB',
      revertible: false,
    })
    expect(byId[1]!.revertible).toBe(true)
    expect(byId[1]!.taskState).toBeNull()
    expect(
      (await endpoints.listConfigChanges({ name: 'WORK_MEM' })).items.map((c) => c.changeId),
    ).toEqual([6, 4, 2])
    expect(
      (await endpoints.listConfigChanges({ level: 'server' })).items.map((c) => c.changeId),
    ).toEqual([4])
    expect(
      (await endpoints.listConfigChanges({ scopeId: 'reports' })).items.map((c) => c.changeId),
    ).toEqual([5])
    const paged = await endpoints.listConfigChanges({ limit: 2 })
    expect(paged.items).toHaveLength(2)
    expect(paged.nextCursor).not.toBeNull()
  })

  it('reverts as a new change and keeps both in the history', async () => {
    await login()
    const task = await endpoints.revertConfigChange(6)
    expect(task.kind).toBe('config.apply')
    expect((await settle(task)).state).toBe('done')
    const p = await param('work_mem')
    expect(p.servers.map((s) => s.value)).toEqual(['16MB', '16MB', '32MB'])
    const list = await endpoints.listConfigChanges({ name: 'work_mem' })
    expect(list.items[0]).toMatchObject({
      reverts: 6,
      oldValue: '8MB',
      newValue: '16MB',
      revertible: true,
      taskState: 'done',
    })
    expect(list.items.find((c) => c.changeId === 6)!.revertible).toBe(false)
  })

  it('puts back "no override" when there was none, and refuses a change that is not the latest', async () => {
    await login()
    await settle(await endpoints.revertConfigChange(4))
    expect((await param('work_mem')).servers[2]).toMatchObject({ value: '8MB', source: 'cluster' })
    const superseded = await problemOf(endpoints.revertConfigChange(2))
    expect(superseded.code).toBe('config_change_superseded')
    expect(superseded.status).toBe(409)
    expect((await problemOf(endpoints.revertConfigChange(999))).status).toBe(404)
    expect((await problemOf(endpoints.revertConfigChange(4))).code).toBe('config_change_superseded')
  })
})

describe('restart', () => {
  it('needs the downtime to be acknowledged', async () => {
    await login()
    const e = await problemOf(endpoints.restartPendingInstances({ acknowledgeDowntime: false }))
    expect(e.code).toBe('validation_failed')
    expect(e.problem.errors?.[0]).toMatchObject({ field: '/acknowledgeDowntime' })
  })

  it('restarts what waits, one server at a time, and the new value runs afterwards', async () => {
    await login()
    const task = await endpoints.restartPendingInstances({ acknowledgeDowntime: true })
    expect(task.kind).toBe('config.restart')
    expect(task.subject).toEqual({ type: 'config', id: 'restart' })
    // Only the server that waits is restarted.
    expect(task.progress.steps.every((s) => s.name.includes('10.0.0.12'))).toBe(true)
    expect((await settle(task)).state).toBe('done')
    const p = await param('shared_buffers')
    expect(p.servers.map((s) => [s.value, s.state])).toEqual([
      ['256MB', 'applied'],
      ['256MB', 'applied'],
      ['256MB', 'applied'],
    ])
    const e = await problemOf(endpoints.restartPendingInstances({ acknowledgeDowntime: true }))
    expect(e.code).toBe('nothing_pending')
  })

  it('starts a standby before its primary when a value goes up', async () => {
    await login()
    await settle(await endpoints.applyConfigChange({ changes: [change('max_connections', '200')] }))
    const task = await endpoints.restartPendingInstances({ acknowledgeDowntime: true })
    const names = task.progress.steps.map((s) => s.name)
    const standbys = names.map((n, i) => (n.includes('(standby)') ? i : -1)).filter((i) => i >= 0)
    const primaries = names.map((n, i) => (n.includes('(primary)') ? i : -1)).filter((i) => i >= 0)
    expect(Math.max(...standbys)).toBeLessThan(Math.min(...primaries))
    await settle(task)
  })

  it('starts the primaries first when a value goes down', async () => {
    await login()
    await settle(await endpoints.applyConfigChange({ changes: [change('max_connections', '50')] }))
    const task = await endpoints.restartPendingInstances({ acknowledgeDowntime: true })
    const names = task.progress.steps.map((s) => s.name)
    const standbys = names.map((n, i) => (n.includes('(standby)') ? i : -1)).filter((i) => i >= 0)
    const primaries = names.map((n, i) => (n.includes('(primary)') ? i : -1)).filter((i) => i >= 0)
    expect(Math.max(...primaries)).toBeLessThan(Math.min(...standbys))
  })

  it('refuses a mixed direction: 409 config_restart_order_conflict', async () => {
    await login()
    seedRestartConflict(getState())
    const e = await problemOf(endpoints.restartPendingInstances({ acknowledgeDowntime: true }))
    expect(e.code).toBe('config_restart_order_conflict')
    expect(e.status).toBe(409)
    expect(e.problem.detail).toMatch(/max_connections.*max_wal_senders/)
  })

  it('restarts only the servers that are named, and does not know a server that does not exist', async () => {
    await login()
    expect(
      (
        await problemOf(
          endpoints.restartPendingInstances({
            acknowledgeDowntime: true,
            serverIds: ['10.0.0.11'],
          }),
        )
      ).code,
    ).toBe('nothing_pending')
    expect(
      (
        await problemOf(
          endpoints.restartPendingInstances({ acknowledgeDowntime: true, serverIds: ['nope'] }),
        )
      ).status,
    ).toBe(404)
    await settle(
      await endpoints.restartPendingInstances({
        acknowledgeDowntime: true,
        serverIds: ['10.0.0.12'],
      }),
    )
  })
})

describe('time', () => {
  it('measures the clocks: the answering server is the reference, one server is 1.82 s ahead', async () => {
    await login()
    const t = await endpoints.getClusterTime()
    expect(t.referenceServerId).toBe('10.0.0.11')
    expect(t.maxDifferenceMs).toBe(1000)
    expect(Math.abs(Date.now() - new Date(t.measuredAt).getTime())).toBeLessThan(5000)
    const [a, b, c] = t.servers
    expect(a).toMatchObject({
      serverId: '10.0.0.11',
      differenceMs: 0,
      uncertaintyMs: 0,
      reachable: true,
    })
    expect(b!.differenceMs).toBeLessThan(b!.uncertaintyMs!)
    expect(c).toMatchObject({
      differenceMs: 1820,
      uncertaintyMs: 0.4,
      osTimeZone: 'UTC',
      timeZone: 'Asia/Tehran',
      logTimeZone: 'Asia/Tehran',
    })
    expect(Math.abs(new Date(c!.clock!).getTime() - Date.now() - 1820)).toBeLessThan(2000)
    expect(t.databases).toEqual([
      { name: 'shop', timeZone: null },
      { name: 'reports', timeZone: 'Europe/Berlin' },
    ])
  })

  it('reports a clock that cannot be read as unreachable with no values, never 0', async () => {
    await login()
    setClockReachable(getState(), '10.0.0.12', false)
    const t = await endpoints.getClusterTime()
    expect(t.servers[1]).toEqual({
      serverId: '10.0.0.12',
      reachable: false,
      clock: null,
      differenceMs: null,
      uncertaintyMs: null,
      roundTripMs: null,
      osTimeZone: null,
      timeZone: null,
      logTimeZone: null,
    })
    const summary = await endpoints.getConfigSummary()
    expect(summary.largestClockDifference.serverId).toBe('10.0.0.13')
    setClockReachable(getState(), '10.0.0.13', false)
    const none = await endpoints.getConfigSummary()
    expect(none.largestClockDifference).toEqual({ serverId: null, differenceMs: null })
  })

  it('lists the time zones PostgreSQL knows, with the offset', async () => {
    await login()
    const zones = (await endpoints.listTimeZones()).items
    expect(zones.length).toBeGreaterThan(200)
    expect(zones.find((z) => z.name === 'Asia/Tehran')).toEqual({
      name: 'Asia/Tehran',
      utcOffset: '+03:30',
      isDaylightSaving: false,
    })
    expect(zones.some((z) => z.isDaylightSaving)).toBe(true)
    expect(zones.every((z) => /^[+-]\d\d:\d\d$/.test(z.utcOffset))).toBe(true)
  })

  it('raises clockDifference above the threshold less the uncertainty, and clears it below', async () => {
    await login()
    let alerts = (await endpoints.listAlerts({ state: 'open' })).items
    const alert = alerts.find((a) => a.kind === 'clockDifference')!
    expect(alert).toMatchObject({
      severity: 'warning',
      serverId: '10.0.0.13',
      subject: { serverId: '10.0.0.13' },
    })
    // Critical at ten times the threshold.
    setClockOffset(getState(), '10.0.0.12', 20000)
    alerts = (await endpoints.listAlerts({ state: 'open' })).items
    expect(
      alerts.find((a) => a.kind === 'clockDifference' && a.serverId === '10.0.0.12'),
    ).toMatchObject({ severity: 'critical' })
    // The uncertainty is taken off: 1000.3 ms with +/- 0.4 ms is not above 1000.
    setClockOffset(getState(), '10.0.0.13', 1000.3)
    setClockOffset(getState(), '10.0.0.12', 0)
    alerts = (await endpoints.listAlerts({ state: 'open' })).items
    expect(alerts.filter((a) => a.kind === 'clockDifference')).toHaveLength(0)
  })

  it('changes the threshold through the cluster settings (200, not a task)', async () => {
    await login()
    expect((await endpoints.getClusterSettings()).clock).toEqual({ maxDifferenceMs: 1000 })
    const updated = await endpoints.updateClusterSettings({ clock: { maxDifferenceMs: 5000 } })
    expect(updated.clock).toEqual({ maxDifferenceMs: 5000 })
    expect((await endpoints.getClusterTime()).maxDifferenceMs).toBe(5000)
    const open = (await endpoints.listAlerts({ state: 'open' })).items
    expect(open.some((a) => a.kind === 'clockDifference')).toBe(false)
    for (const bad of [9, 600001, 1.5]) {
      const e = await problemOf(
        endpoints.updateClusterSettings({ clock: { maxDifferenceMs: bad } }),
      )
      expect(e.code).toBe('validation_failed')
    }
  })
})

describe('summary and servers', () => {
  it('counts what the dashboard shows, with the names', async () => {
    await login()
    const s = await endpoints.getConfigSummary()
    expect(s).toEqual({
      revision: 6,
      serversPendingRestart: ['10.0.0.12'],
      differingInGroup: ['checkpoint_timeout', 'shared_buffers'],
      failedServerIds: [],
      waitingServerIds: [],
      largestClockDifference: { serverId: '10.0.0.13', differenceMs: 1820 },
      maxDifferenceMs: 1000,
    })
  })

  it('has a state per server with its group, revision and overrides', async () => {
    await login()
    const list = await endpoints.listConfigServers()
    expect(list.revision).toBe(6)
    expect(list.items.map((s) => [s.serverId, s.serverGroup, s.state, s.appliedRevision])).toEqual([
      ['10.0.0.11', 'rack-a', 'applied', 6],
      ['10.0.0.12', 'rack-a', 'pendingRestart', 6],
      ['10.0.0.13', 'rack-b', 'applied', 6],
    ])
    expect(list.items[1]!.pendingRestart).toEqual(['shared_buffers'])
    // cluster: shared_buffers, work_mem; group rack-a: effective_io_concurrency; server .13: work_mem
    expect(list.items.map((s) => s.overrideCount)).toEqual([3, 3, 3])
  })

  it('follows a server that changes group', async () => {
    await login()
    getState().servers.find((s) => s.id === '10.0.0.13')!.serverGroup = 'rack-a'
    const p = await param('effective_io_concurrency')
    expect(p.servers[2]!.level).toBe('serverGroup')
  })
})

describe('who may do what', () => {
  it('lets a read-only account read and refuses it the preview, the change and the restart', async () => {
    await login('viewer')
    expect((await endpoints.listConfigParameters({ limit: 5 })).items).toHaveLength(5)
    await endpoints.getConfigSummary()
    await endpoints.getClusterTime()
    await endpoints.listTimeZones()
    await endpoints.listConfigChanges()
    await endpoints.listConfigServers()
    for (const call of [
      () => endpoints.previewConfigChange({ changes: [change('work_mem', '16MB')] }),
      () => endpoints.applyConfigChange({ changes: [change('work_mem', '16MB')] }),
      () => endpoints.revertConfigChange(6),
      () => endpoints.restartPendingInstances({ acknowledgeDowntime: true }),
    ])
      expect((await problemOf(call())).code).toBe('forbidden')
  })

  it('refuses an operator the change as well, and lets an administrator do it', async () => {
    await login('ops')
    expect(
      (await problemOf(endpoints.applyConfigChange({ changes: [change('work_mem', '16MB')] })))
        .code,
    ).toBe('forbidden')
    await login('admin')
    expect(
      (await settle(await endpoints.applyConfigChange({ changes: [change('work_mem', '16MB')] })))
        .state,
    ).toBe('done')
  })

  it('refuses an account with only the metrics role every read', async () => {
    await login('metrics')
    for (const call of [
      () => endpoints.listConfigParameters(),
      () => endpoints.getConfigParameter('work_mem'),
      () => endpoints.getConfigSummary(),
      () => endpoints.getClusterTime(),
      () => endpoints.listTimeZones(),
      () => endpoints.listConfigServers(),
      () => endpoints.listConfigChanges(),
    ])
      expect((await problemOf(call())).code).toBe('forbidden')
  })

  it('answers unsupported_operation for what the server does not implement', async () => {
    await login()
    const { mockConfig } = await import('@/mocks/state')
    mockConfig.unsupported.add('listConfigParameters')
    expect((await problemOf(endpoints.listConfigParameters())).code).toBe('unsupported_operation')
    await endpoints.getConfigSummary()
  })
})

describe('the warnings of a preview (contract 0.17.0)', () => {
  it('flags a memory value above ten times or below a tenth of the default, not exactly ten times or a tenth', async () => {
    await login()
    // work_mem: default 4MB (4096 kB).
    const w = async (v: string) =>
      (await endpoints.previewConfigChange({ changes: [change('work_mem', v)] })).warnings ?? []
    expect(await w('16MB')).toEqual([])
    expect(await w('40MB')).toEqual([])
    const high = await w('41MB')
    expect(high).toHaveLength(1)
    expect(high[0]).toMatchObject({
      index: 0,
      pointer: '/changes/0/value',
      code: 'config_value_unusual',
    })
    expect((await w('409kB'))[0]?.code).toBe('config_value_unusual')
    expect(await w('410kB')).toEqual([])
  })

  it('never flags a parameter that is not a memory size, and a removal', async () => {
    await login()
    const none = await endpoints.previewConfigChange({
      changes: [change('statement_timeout', '24h'), change('work_mem', null)],
    })
    expect(none.valid).toBe(true)
    expect(none.warnings ?? []).toEqual([])
  })

  it('warns when the reserved connections leave no room, and the preview stays valid', async () => {
    await login()
    const r = await endpoints.previewConfigChange({
      changes: [change('superuser_reserved_connections', '100')],
    })
    expect(r.valid).toBe(true)
    expect(r.problems).toEqual([])
    expect(r.warnings).toHaveLength(1)
    expect(r.warnings![0]).toMatchObject({ code: 'config_reserved_connections' })
    // 3 + 96 + 1 = 100 is allowed.
    const edge = await endpoints.previewConfigChange({
      changes: [change('reserved_connections', '96')],
    })
    expect(edge.warnings ?? []).toEqual([])
    const over = await endpoints.previewConfigChange({
      changes: [change('reserved_connections', '97')],
    })
    expect(over.warnings).toHaveLength(1)
  })

  it('gives no warnings for an invalid request', async () => {
    await login()
    const r = await endpoints.previewConfigChange({ changes: [change('work_mem', '1GBx')] })
    expect(r.valid).toBe(false)
    expect(r.warnings ?? []).toEqual([])
  })
})
