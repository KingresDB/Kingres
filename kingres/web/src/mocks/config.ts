// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The eleven operations of the tag `Configuration` (contract 0.8.0) for the mock API. They follow what the design of
// phase 13 says the real server does, and are never more permissive: the same checks in the same order (names and
// levels, then type, unit and range with the shared checker in `lib/config-value.ts`, then the floors), a mutation is a
// task with a step per server, a server that fails does not stop the others, a server that cannot be reached holds the
// task until it is back, and nothing is restarted by a change.
import type { components } from '@/api/schema'
import type { OperationId } from '@/api/operations.generated'
import type {
  ConfigChange,
  ConfigChangeItem,
  ConfigChangePreview,
  ConfigEffect,
  ConfigParameter,
  ConfigProblem,
  ConfigRestartStep,
  ConfigServerStatus,
  ConfigServerValue,
  ConfigWarning,
  Task,
  ConfigLevel,
} from '@/api/types'
import { baseNumber, checkConfigValue, specOf } from '@/lib/config-value'
import { accepted, ok, paginate, problem, validationFailed, type Ctx, type Handler } from './http'
import {
  bootText,
  lowerName,
  wantedValue,
  type ConfigState,
  type ParamDef,
  type ServerCfg,
} from './config-data'
import { TIME_ZONES, ZONE_NAMES } from './config-zones'
import {
  activeTask,
  createTask,
  findTask,
  instancesOfServer,
  mockConfig,
  type MockState,
} from './state'

type S = components['schemas']

/** The five settings a hot standby needs at least as high as its primary (PostgreSQL's `CheckRequiredParameterValues`). */
export const STANDBY_FLOOR_SETTINGS = [
  'max_connections',
  'max_worker_processes',
  'max_wal_senders',
  'max_prepared_transactions',
  'max_locks_per_transaction',
] as const

const body = <T>(ctx: Ctx): T => (ctx.body ?? {}) as T

// --- state helpers ------------------------------------------------------------------------------

/** Brings the configuration state in line with the servers of the cluster (a server added since, a group changed). */
function sync(s: MockState): ConfigState {
  const cfg = s.config
  const template = cfg.servers['10.0.0.11']
  const base = cfg.base['10.0.0.11']
  for (const rec of s.servers) {
    const known = cfg.servers[rec.id]
    if (known) {
      known.serverGroup = rec.serverGroup
      continue
    }
    if (!template || !base) continue
    // A new server starts with the cluster's values already applied: it gets the file when it joins.
    cfg.base[rec.id] = structuredClone(base)
    const fresh: ServerCfg = {
      serverGroup: rec.serverGroup,
      appliedRevision: cfg.revision,
      run: {},
      pending: {},
      failed: {},
      waiting: false,
      waitDetail: null,
    }
    cfg.servers[rec.id] = fresh
    for (const p of cfg.params) {
      const w = wantedValue(cfg, rec.id, p.name)
      fresh.run[p.name] = { value: w.value, source: w.source }
    }
    cfg.clocks[rec.id] = {
      offsetMs: 0.2,
      uncertaintyMs: 0.4,
      roundTripMs: 0.6,
      osTimeZone: cfg.clocks['10.0.0.11']?.osTimeZone ?? null,
      reachable: true,
    }
  }
  return cfg
}

const paramOf = (cfg: ConfigState, name: string): ParamDef | undefined =>
  cfg.byLower.get(lowerName(name))

type Perhaps = ConfigServerValue['state']

function stateOf(cfg: ConfigState, serverId: string, srv: ServerCfg, def: ParamDef): Perhaps {
  if (srv.failed[def.name] !== undefined) return 'failed'
  // A server that cannot be reached has not taken the values that differ from what it runs.
  if (
    srv.waiting &&
    srv.pending[def.name] === undefined &&
    wantedValue(cfg, serverId, def.name).value !== srv.run[def.name]?.value
  )
    return 'waiting'
  if (srv.pending[def.name] !== undefined) return 'pendingRestart'
  return 'applied'
}

function serverValue(
  cfg: ConfigState,
  def: ParamDef,
  serverId: string,
  missing: ReadonlySet<string>,
): ConfigServerValue {
  const srv = cfg.servers[serverId] as ServerCfg
  if (missing.has(serverId)) {
    return {
      serverId,
      value: null,
      source: 'other',
      level: null,
      state: 'unknown',
      pendingValue: null,
      error: null,
    }
  }
  const run = srv.run[def.name]
  const state = stateOf(cfg, serverId, srv, def)
  return {
    serverId,
    value: def.hidden ? null : (run?.value ?? null),
    source: run?.source ?? 'default',
    level: wantedValue(cfg, serverId, def.name).level,
    state,
    pendingValue: def.hidden || state !== 'pendingRestart' ? null : (srv.pending[def.name] ?? null),
    error: state === 'failed' ? (srv.failed[def.name] ?? null) : null,
  }
}

function parameterView(s: MockState, def: ParamDef): ConfigParameter {
  const cfg = s.config
  const missing = new Set((mockConfig.partial ?? []).map((m) => m.serverId))
  const servers = s.servers.map((x) => serverValue(cfg, def, x.id, missing))
  const known = servers.filter((x) => x.state !== 'unknown')
  const values = new Set(known.map((x) => x.value))
  const differs = !def.hidden && values.size > 1
  const overrides = cfg.overrides
    .filter((o) => lowerName(o.name) === lowerName(def.name))
    .map((o) => ({
      level: o.level,
      scopeId: o.scopeId,
      value: o.value,
      revision: o.revision,
      updatedBy: o.updatedBy,
      updatedAt: o.updatedAt,
    }))
  return {
    name: def.name,
    category: def.category,
    description: def.description,
    type: def.type,
    unit: def.unit,
    min: def.min,
    max: def.max,
    enumValues: def.enumValues,
    context: def.context,
    restartRequired: def.context === 'postmaster',
    defaultValue: def.hidden ? null : bootText(def),
    kind: def.kind,
    reason: def.reason,
    hidden: def.hidden,
    allowedLevels: def.allowedLevels,
    clusterOnly: def.clusterOnly,
    overrides: def.hidden ? [] : overrides,
    servers,
    differs,
    differsInGroup: differs && differsInGroup(cfg, def, s),
  }
}

/** The value differs between servers of one server group and no server-level override explains it. */
function differsInGroup(cfg: ConfigState, def: ParamDef, s: MockState): boolean {
  if (def.kind !== 'settable') return false
  const groups = new Map<string, Set<string>>()
  for (const rec of s.servers) {
    if (rec.serverGroup === null) continue
    const set = groups.get(rec.serverGroup) ?? new Set<string>()
    set.add(cfg.servers[rec.id]?.run[def.name]?.value ?? '')
    groups.set(rec.serverGroup, set)
  }
  const someGroupDiffers = [...groups.entries()].some(([group, set]) => {
    if (set.size < 2) return false
    // A server-level override on a member of this group explains the difference.
    const explained = cfg.overrides.some(
      (o) =>
        o.level === 'server' &&
        lowerName(o.name) === lowerName(def.name) &&
        s.servers.some((x) => x.id === o.scopeId && x.serverGroup === group),
    )
    return !explained
  })
  return someGroupDiffers
}

// --- listing parameters ---------------------------------------------------------------------------

function listParameters(c: Ctx): ReturnType<Handler> {
  const s = c.s
  const cfg = sync(s)
  const q = c.url.searchParams
  const text = q.get('q')?.toLowerCase()
  const category = q.get('category')
  const bool = (n: string) => q.get(n) === 'true' || q.get(n) === '1'
  const kind = q.get('kind')
  const state = q.get('state')
  let items = cfg.params.map((p) => ({ def: p, view: undefined as ConfigParameter | undefined }))
  const view = (e: { def: ParamDef; view: ConfigParameter | undefined }): ConfigParameter => {
    e.view ??= parameterView(s, e.def)
    return e.view
  }
  items = items.filter((e) => {
    const p = e.def
    if (text && !p.name.toLowerCase().includes(text) && !p.description.toLowerCase().includes(text))
      return false
    if (category && p.category !== category && !p.category.startsWith(`${category} /`)) return false
    if (kind && p.kind !== kind) return false
    if (bool('changed') && !cfg.overrides.some((o) => lowerName(o.name) === lowerName(p.name)))
      return false
    if (bool('differing') && !view(e).differs) return false
    if (bool('pendingRestart') && !view(e).servers.some((x) => x.state === 'pendingRestart'))
      return false
    if (state && !view(e).servers.some((x) => x.state === state)) return false
    return true
  })
  const page = paginate(items, c.url)
  return ok({
    items: page.items.map(view),
    nextCursor: page.nextCursor,
    revision: cfg.revision,
    ...(mockConfig.partial
      ? { partial: true, missingServers: mockConfig.partial }
      : { partial: false, missingServers: [] }),
  } satisfies S['ConfigParameterList'])
}

// --- validation, shared by the preview and the change --------------------------------------------------

interface Checked {
  index: number
  def: ParamDef
  level: ConfigLevel
  scopeId: string | null
  /** Normalized text, or null to remove the value at this level. */
  value: string | null
}

function floorOf(s: MockState, name: string): { min: number; why: string } | null {
  const replicas = s.settings.defaultReplicaCount
  if (name === 'max_prepared_transactions')
    return { min: 1, why: 'kingres_shard needs two-phase commit, so at least 1.' }
  if (name === 'max_wal_senders')
    return {
      min: 4 + replicas,
      why: `every standby holds a sender and base backups take one more: at least 4 + the replica count (${replicas}).`,
    }
  if (name === 'max_replication_slots')
    return {
      min: 2 + replicas,
      why: `every standby holds a slot: at least 2 + the replica count (${replicas}).`,
    }
  return null
}

function checkChanges(
  s: MockState,
  items: ConfigChangeItem[],
): { checked: Checked[]; problems: ConfigProblem[] } {
  const cfg = sync(s)
  const checked: Checked[] = []
  const problems: ConfigProblem[] = []
  items.forEach((item, index) => {
    const at = (field: string, code: string, message: string): void => {
      problems.push({ index, pointer: `/changes/${index}/${field}`, code, message })
    }
    const def = paramOf(cfg, item.name)
    if (!def) {
      at('name', 'config_parameter_unknown', `The server has no parameter called "${item.name}".`)
      return
    }
    if (def.kind === 'managed') {
      at('name', 'config_parameter_managed', def.reason ?? 'KingresDB manages this parameter.')
      return
    }
    if (def.kind === 'refused') {
      at('name', 'config_parameter_refused', def.reason ?? 'The console never sets this parameter.')
      return
    }
    if (!def.allowedLevels.includes(item.level)) {
      at(
        'level',
        'config_level_not_allowed',
        def.clusterOnly && (item.level === 'serverGroup' || item.level === 'server')
          ? `${def.clusterOnly} Set it for the cluster.`
          : `${def.name} cannot be set at the ${item.level} level.`,
      )
      return
    }
    let scopeId: string | null = null
    if (item.level !== 'cluster') {
      scopeId = item.scopeId ?? null
      if (scopeId === null || scopeId === '') {
        at('scopeId', 'not_found', `A ${item.level} is required for this level.`)
        return
      }
      const exists =
        item.level === 'serverGroup'
          ? s.serverGroups.some((g) => g.name === scopeId)
          : item.level === 'server'
            ? s.servers.some((x) => x.id === scopeId)
            : s.databases.some((d) => d.info.name === scopeId)
      if (!exists) {
        at('scopeId', 'not_found', `There is no ${item.level} called "${scopeId}".`)
        return
      }
    }
    if (item.value === null || item.value === undefined) {
      checked.push({ index, def, level: item.level, scopeId, value: null })
      return
    }
    const result = checkConfigValue(item.value, specOf(def))
    if (!result.ok) {
      at('value', 'config_value_invalid', result.message)
      return
    }
    // Layer 3: PostgreSQL's own check of the time zone names.
    if (def.name === 'TimeZone' || def.name === 'log_timezone') {
      const known = TIME_ZONES.find((z) => z.name.toLowerCase() === result.normalized.toLowerCase())
      if (!known || !ZONE_NAMES.has(known.name)) {
        at('value', 'config_value_invalid', `time zone "${result.normalized}" not recognized`)
        return
      }
      checked.push({ index, def, level: item.level, scopeId, value: known.name })
      return
    }
    const floor = floorOf(s, def.name)
    if (floor && item.level !== 'database' && Number(result.base) < floor.min) {
      at(
        'value',
        'config_value_below_floor',
        `${def.name} must be at least ${floor.min}: ${floor.why}`,
      )
      return
    }
    checked.push({ index, def, level: item.level, scopeId, value: result.normalized })
  })
  return { checked, problems }
}

/** The error of a change that is refused: the same codes and statuses as the contract lists for `applyConfigChange`. */
function refuse(problems: ConfigProblem[]): never {
  const first = problems[0] as ConfigProblem
  if (problems.every((p) => p.code === 'config_value_invalid'))
    throw validationFailed(
      problems.map((p) => ({ field: p.pointer, message: p.message, code: p.code })),
    )
  const code = first.code
  if (
    code === 'config_parameter_managed' ||
    code === 'config_parameter_refused' ||
    code === 'config_level_not_allowed' ||
    code === 'config_value_below_floor' ||
    code === 'config_parameter_unknown'
  )
    throw problem(code, first.message)
  if (code === 'not_found') throw problem('not_found', first.message)
  throw validationFailed(problems.map((p) => ({ field: p.pointer, message: p.message })))
}

// --- the effect of a change ---------------------------------------------------------------------------

function applyToOverrides(
  cfg: ConfigState,
  checked: Checked[],
  user: string,
  revision: number,
): ConfigState['overrides'] {
  const next = cfg.overrides.map((o) => ({ ...o }))
  for (const c of checked) {
    const at = next.findIndex(
      (o) =>
        lowerName(o.name) === lowerName(c.def.name) &&
        o.level === c.level &&
        o.scopeId === c.scopeId,
    )
    if (at >= 0) next.splice(at, 1)
    if (c.value !== null)
      next.push({
        level: c.level,
        scopeId: c.scopeId,
        name: c.def.name,
        value: c.value,
        revision,
        updatedBy: user,
        updatedAt: new Date().toISOString(),
      })
  }
  return next
}

function serversOf(s: MockState, level: ConfigLevel, scopeId: string | null): string[] {
  if (level === 'cluster' || level === 'database') return s.servers.map((x) => x.id)
  if (level === 'server') return s.servers.filter((x) => x.id === scopeId).map((x) => x.id)
  return s.servers.filter((x) => x.serverGroup === scopeId).map((x) => x.id)
}

function effectsOf(s: MockState, checked: Checked[]): ConfigEffect[] {
  const cfg = s.config
  const next = applyToOverrides(cfg, checked, 'preview', cfg.revision + 1)
  const out: ConfigEffect[] = []
  const seen = new Set<string>()
  for (const c of checked) {
    for (const id of serversOf(s, c.level, c.scopeId)) {
      const key = `${id}|${c.def.name}|${c.level}`
      if (seen.has(key)) continue
      seen.add(key)
      const srv = cfg.servers[id] as ServerCfg
      if (c.level === 'database') {
        const old = cfg.overrides.find(
          (o) =>
            o.level === 'database' &&
            o.scopeId === c.scopeId &&
            lowerName(o.name) === lowerName(c.def.name),
        )?.value
        out.push({
          serverId: id,
          name: c.def.name,
          before: old ?? null,
          after: c.value,
          takesEffect: cfg.inject.downServers.has(id)
            ? 'waits'
            : (old ?? null) === c.value
              ? 'unchanged'
              : 'reload',
        })
        continue
      }
      const before = srv.run[c.def.name]?.value ?? null
      const after = wantedValue(cfg, id, c.def.name, next).value
      let takesEffect: ConfigEffect['takesEffect']
      if (cfg.inject.downServers.has(id)) takesEffect = 'waits'
      else if (after === before && srv.pending[c.def.name] === undefined) takesEffect = 'unchanged'
      else takesEffect = c.def.context === 'postmaster' ? 'restart' : 'reload'
      out.push({ serverId: id, name: c.def.name, before, after, takesEffect })
    }
  }
  return out
}

/** Which way each of the five settings goes, from the effects of a change or from what waits for a restart. */
function directions(
  cfg: ConfigState,
  moves: { name: string; before: string | null; after: string | null }[],
): { up: string[]; down: string[] } {
  const up = new Set<string>()
  const down = new Set<string>()
  for (const m of moves) {
    if (!(STANDBY_FLOOR_SETTINGS as readonly string[]).includes(m.name)) continue
    const def = paramOf(cfg, m.name)
    if (!def || m.before === null || m.after === null) continue
    const a = baseNumber(m.before, specOf(def))
    const b = baseNumber(m.after, specOf(def))
    if (a === null || b === null || a === b) continue
    ;(b > a ? up : down).add(m.name)
  }
  return { up: [...up], down: [...down] }
}

function conflictText(dir: { up: string[]; down: string[] }): string | null {
  if (dir.up.length === 0 || dir.down.length === 0) return null
  return `${dir.up.join(', ')} would go up and ${dir.down.join(', ')} would go down. No restart order keeps every standby at or above its primary. Change one direction at a time.`
}

/** The instances of the given servers in the order a restart follows (a value up: standbys first; down: primaries first). */
function restartOrderOf(
  s: MockState,
  serverIds: string[],
  dir: { up: string[]; down: string[] },
): ConfigRestartStep[] {
  const steps: ConfigRestartStep[] = []
  for (const id of [...serverIds].sort()) {
    for (const i of instancesOfServer(s, id))
      steps.push({
        serverId: id,
        instanceId: i.id,
        role: i.role === 'primary' ? 'primary' : 'standby',
      })
  }
  const rank = (r: ConfigRestartStep): number =>
    dir.down.length > 0 && dir.up.length === 0
      ? r.role === 'primary'
        ? 0
        : 1
      : r.role === 'standby'
        ? 0
        : 1
  return steps.sort(
    (a, b) => rank(a) - rank(b) || (a.serverId < b.serverId ? -1 : a.serverId > b.serverId ? 1 : 0),
  )
}

const MEMORY_UNITS = ['B', 'kB', '8kB']
const RESERVED_SETTINGS = [
  'max_connections',
  'superuser_reserved_connections',
  'reserved_connections',
]

/** The warnings of a valid preview: a memory value far from its default, and reserved connections that leave no room. */
function warningsOf(s: MockState, checked: Checked[]): ConfigWarning[] {
  const out: ConfigWarning[] = []
  const at = (index: number, code: string, message: string): void => {
    out.push({ index, pointer: `/changes/${index}/value`, code, message })
  }
  for (const c of checked) {
    const def = c.def
    if (c.value === null || def.type !== 'integer' || !MEMORY_UNITS.includes(def.unit ?? ''))
      continue
    const boot = Number(def.bootBase)
    const r = checkConfigValue(c.value, specOf(def))
    if (!r.ok || !(boot > 0)) continue
    const base = Number(r.base)
    if (base > boot * 10)
      at(
        c.index,
        'config_value_unusual',
        `${def.name} is more than ten times its default; this is unusual.`,
      )
    else if (base < boot / 10)
      at(
        c.index,
        'config_value_unusual',
        `${def.name} is below a tenth of its default; this is unusual.`,
      )
  }
  const touched = checked.filter((c) => RESERVED_SETTINGS.includes(c.def.name))
  if (touched.length > 0) {
    const cfg = s.config
    const next = applyToOverrides(cfg, checked, 'preview', cfg.revision + 1)
    const last = touched[touched.length - 1] as Checked
    for (const id of Object.keys(cfg.servers).sort()) {
      const v = (name: string): number => Number(wantedValue(cfg, id, name, next).value)
      const max = v('max_connections')
      const su = v('superuser_reserved_connections')
      const rs = v('reserved_connections')
      if ([max, su, rs].some((n) => !Number.isFinite(n))) continue
      if (max < su + rs + 1) {
        at(
          last.index,
          'config_reserved_connections',
          `On server ${id} max_connections (${max}) would be below superuser_reserved_connections (${su}) plus reserved_connections (${rs}) plus 1; PostgreSQL would not start.`,
        )
        break
      }
    }
  }
  return out
}

function preview(c: Ctx): ReturnType<Handler> {
  const req = body<S['ConfigChangeRequest']>(c)
  if (!Array.isArray(req.changes) || req.changes.length === 0)
    throw validationFailed([{ field: '/changes', message: 'Send at least one change.' }])
  const { checked, problems } = checkChanges(c.s, req.changes)
  const effects = problems.length > 0 ? [] : effectsOf(c.s, checked)
  const restartServerIds = [
    ...new Set(effects.filter((e) => e.takesEffect === 'restart').map((e) => e.serverId)),
  ]
  const dir = directions(c.s.config, effects)
  const conflict = conflictText(dir)
  return ok({
    valid: problems.length === 0,
    problems,
    warnings: problems.length > 0 ? [] : warningsOf(c.s, checked),
    effects,
    restartServerIds,
    restartOrder: conflict ? [] : restartOrderOf(c.s, restartServerIds, dir),
    restartOrderConflict: conflict,
  } satisfies ConfigChangePreview)
}

// --- applying a change: the task config.apply ----------------------------------------------------------

function ensureNoConfigTask(s: MockState): void {
  if (activeTask(s, 'config.apply') || activeTask(s, 'config.restart'))
    throw problem('operation_in_progress', 'A configuration task is running.')
}

/** Moves one server to what the stored overrides want (the work of `POST /internal/v1/config/apply`). */
function reconcile(
  cfg: ConfigState,
  serverId: string,
  failing: string | undefined,
  touched: ParamDef[],
): void {
  const srv = cfg.servers[serverId] as ServerCfg
  const failName =
    failing === undefined
      ? undefined
      : (touched.find((p) => p.context !== 'postmaster') ?? touched[0])?.name
  for (const def of cfg.params) {
    const w = wantedValue(cfg, serverId, def.name)
    const cur = srv.run[def.name]
    if (failName === def.name && failing !== undefined) {
      srv.failed[def.name] = failing
      continue
    }
    delete srv.failed[def.name]
    if (cur && cur.value === w.value) {
      delete srv.pending[def.name]
      srv.run[def.name] = { value: w.value, source: w.source }
    } else if (def.context === 'postmaster') {
      srv.pending[def.name] = w.value
    } else {
      delete srv.pending[def.name]
      srv.run[def.name] = { value: w.value, source: w.source }
    }
  }
  srv.waiting = false
  srv.waitDetail = null
  if (failName === undefined) srv.appliedRevision = cfg.revision
}

function startApply(
  s: MockState,
  checked: Checked[],
  user: string,
  reverts: number | null,
  label: 'apply' | 'revert',
): Task {
  void label
  const cfg = sync(s)
  ensureNoConfigTask(s)
  const changed = checked.filter((c) => {
    const current = cfg.overrides.find(
      (o) =>
        o.level === c.level &&
        o.scopeId === c.scopeId &&
        lowerName(o.name) === lowerName(c.def.name),
    )
    return (current?.value ?? null) !== c.value
  })
  let revision = cfg.revision
  const newChanges: ConfigState['changes'] = []
  if (changed.length > 0) {
    revision += 1
    for (const c of changed) {
      const current = cfg.overrides.find(
        (o) =>
          o.level === c.level &&
          o.scopeId === c.scopeId &&
          lowerName(o.name) === lowerName(c.def.name),
      )
      newChanges.push({
        changeId: cfg.nextChangeId++,
        revision,
        name: c.def.name,
        level: c.level,
        scopeId: c.scopeId,
        oldValue: current?.value ?? null,
        newValue: c.value,
        changedBy: user,
        changedAt: new Date().toISOString(),
        taskId: null,
        reverts,
      })
    }
    cfg.overrides = applyToOverrides(cfg, changed, user, revision)
    cfg.revision = revision
    cfg.changes.push(...newChanges)
  }
  const ids = s.servers.map((x) => x.id)
  const steps = [
    'Validate the change',
    `Record revision ${cfg.revision}`,
    ...ids.map((id) => `Apply on server ${id}`),
    'Confirm',
  ]
  const touched = [...new Set(checked.map((c) => c.def))]
  // The one failure of a server is spent now: a retry works.
  const failing = new Map<string, string>()
  for (const id of ids) {
    const text = cfg.inject.failServers.get(id)
    if (text !== undefined) {
      failing.set(id, text)
      cfg.inject.failServers.delete(id)
    }
  }
  const done = new Set<string>()
  const down = ids.find((id) => cfg.inject.downServers.has(id))
  for (const id of ids) {
    if (cfg.inject.downServers.has(id)) {
      const srv = cfg.servers[id] as ServerCfg
      srv.waiting = true
      srv.waitDetail = 'The instance is not running; the server gets the change when it returns.'
    }
  }
  const failedList = [...failing.keys()]
  const failedFirst = failedList[0]
  const task = createTask(s, {
    kind: 'config.apply',
    subject: { type: 'config', id: 'cluster' },
    steps,
    detail: null,
    cancellable: down !== undefined,
    ...(failedFirst !== undefined
      ? {
          failAtEnd: {
            code: 'config_incomplete',
            message: `Applied on ${ids.filter((x) => !failing.has(x) && !cfg.inject.downServers.has(x)).join(', ') || 'no server'}; failed on ${failedList.join(', ')}: ${failing.get(failedFirst) ?? ''}`,
            step: `Apply on server ${failedFirst}`,
            detail: failing.get(failedFirst) ?? '',
          },
        }
      : {}),
    apply: () => {
      for (const id of ids) {
        if (!done.has(id) && !cfg.inject.downServers.has(id)) {
          reconcile(cfg, id, failing.get(id), touched)
          done.add(id)
        }
      }
    },
    onProgress: (fraction) => {
      ids.forEach((id, k) => {
        if (done.has(id) || cfg.inject.downServers.has(id)) return
        if (fraction >= (2 + k + 0.5) / steps.length) {
          reconcile(cfg, id, failing.get(id), touched)
          done.add(id)
        }
      })
    },
  })
  for (const ch of newChanges) ch.taskId = task.id
  if (down !== undefined) {
    const rec = findTask(s, task.id)
    if (rec) {
      rec.holdAt = (2 + ids.indexOf(down) + 0.5) / steps.length
      rec.holdDetail = `Waiting for server ${down}, which cannot be reached`
    }
  }
  return task
}

function applyChange(c: Ctx): ReturnType<Handler> {
  const req = body<S['ConfigChangeRequest']>(c)
  if (!Array.isArray(req.changes) || req.changes.length === 0)
    throw validationFailed([{ field: '/changes', message: 'Send at least one change.' }])
  if (req.changes.length > 100)
    throw validationFailed([{ field: '/changes', message: 'At most 100 changes in one request.' }])
  const { checked, problems } = checkChanges(c.s, req.changes)
  if (problems.length > 0) refuse(problems)
  return accepted(startApply(c.s, checked, c.s.session?.username ?? 'system', null, 'apply'))
}

// --- history ----------------------------------------------------------------------------------------------

function isRevertible(cfg: ConfigState, ch: ConfigState['changes'][number]): boolean {
  return !cfg.changes.some(
    (x) =>
      x.changeId > ch.changeId &&
      x.name === ch.name &&
      x.level === ch.level &&
      x.scopeId === ch.scopeId,
  )
}

function changeView(s: MockState, ch: ConfigState['changes'][number]): ConfigChange {
  const task = ch.taskId === null ? undefined : s.tasks.find((t) => t.task.id === ch.taskId)
  return {
    changeId: ch.changeId,
    revision: ch.revision,
    name: ch.name,
    level: ch.level,
    scopeId: ch.scopeId,
    oldValue: ch.oldValue,
    newValue: ch.newValue,
    changedBy: ch.changedBy,
    changedAt: ch.changedAt,
    taskId: ch.taskId,
    reverts: ch.reverts,
    revertible: isRevertible(s.config, ch),
    taskState: task ? (task.task.state === 'paused' ? 'failed' : task.task.state) : null,
  }
}

function listChanges(c: Ctx): ReturnType<Handler> {
  const cfg = sync(c.s)
  const q = c.url.searchParams
  const name = q.get('name')?.toLowerCase()
  const level = q.get('level')
  const scopeId = q.get('scopeId')
  const items = [...cfg.changes]
    .filter(
      (x) =>
        (!name || x.name.toLowerCase() === name) &&
        (!level || x.level === level) &&
        (!scopeId || x.scopeId === scopeId),
    )
    .sort((a, b) => b.changeId - a.changeId)
  const page = paginate(items, c.url)
  return ok({ items: page.items.map((x) => changeView(c.s, x)), nextCursor: page.nextCursor })
}

function revert(c: Ctx): ReturnType<Handler> {
  const cfg = sync(c.s)
  const id = Number(c.params.changeId)
  const ch = cfg.changes.find((x) => x.changeId === id)
  if (!ch) throw problem('not_found', `Change ${c.params.changeId} does not exist.`)
  if (!isRevertible(cfg, ch))
    throw problem(
      'config_change_superseded',
      'A later change touched the same parameter at the same level and scope. Revert the later one first.',
    )
  const { checked, problems } = checkChanges(c.s, [
    { name: ch.name, level: ch.level, scopeId: ch.scopeId, value: ch.oldValue },
  ])
  if (problems.length > 0) refuse(problems)
  return accepted(
    startApply(c.s, checked, c.s.session?.username ?? 'system', ch.changeId, 'revert'),
  )
}

// --- servers, summary, time --------------------------------------------------------------------------------

function serverStatus(s: MockState, id: string): ConfigServerStatus {
  const cfg = s.config
  const srv = cfg.servers[id] as ServerCfg
  const pendingRestart = Object.keys(srv.pending).sort()
  const failed = Object.entries(srv.failed).map(([name, error]) => ({ name, error }))
  const state: ConfigServerStatus['state'] =
    failed.length > 0
      ? 'failed'
      : srv.waiting
        ? 'waiting'
        : pendingRestart.length > 0
          ? 'pendingRestart'
          : 'applied'
  const rec = s.servers.find((x) => x.id === id)
  const applies = cfg.overrides.filter(
    (o) =>
      o.level === 'cluster' ||
      (o.level === 'server' && o.scopeId === id) ||
      (o.level === 'serverGroup' && o.scopeId === rec?.serverGroup),
  )
  return {
    serverId: id,
    serverGroup: rec?.serverGroup ?? null,
    appliedRevision: srv.appliedRevision,
    state,
    pendingRestart,
    failed,
    overrideCount: applies.length,
    detail: state === 'waiting' ? srv.waitDetail : null,
  }
}

function summary(c: Ctx): ReturnType<Handler> {
  const s = c.s
  const cfg = sync(s)
  const statuses = s.servers.map((x) => serverStatus(s, x.id))
  const differing = cfg.params
    .filter((p) => !p.hidden && differsInGroup(cfg, p, s))
    .map((p) => p.name)
    .slice(0, 50)
  const times = timeViews(s).filter((t) => t.reachable && t.serverId !== ANSWER)
  let largest: ServerTimeLike | undefined
  for (const t of times)
    if (!largest || Math.abs(t.differenceMs ?? 0) > Math.abs(largest.differenceMs ?? 0)) largest = t
  return ok({
    revision: cfg.revision,
    serversPendingRestart: statuses
      .filter((x) => x.pendingRestart.length > 0)
      .map((x) => x.serverId),
    differingInGroup: differing,
    failedServerIds: statuses.filter((x) => x.state === 'failed').map((x) => x.serverId),
    waitingServerIds: statuses.filter((x) => x.state === 'waiting').map((x) => x.serverId),
    largestClockDifference: {
      serverId: largest?.serverId ?? null,
      differenceMs: largest?.differenceMs ?? null,
    },
    maxDifferenceMs: s.settings.clock?.maxDifferenceMs ?? 1000,
  } satisfies S['ConfigSummary'])
}

const ANSWER = '10.0.0.11'
type ServerTimeLike = S['ServerTime']

function timeViews(s: MockState): ServerTimeLike[] {
  const cfg = sync(s)
  const now = Date.now()
  return s.servers.map((rec) => {
    const clock = cfg.clocks[rec.id]
    const srv = cfg.servers[rec.id]
    if (!clock || !clock.reachable || !srv) {
      return {
        serverId: rec.id,
        reachable: false,
        clock: null,
        differenceMs: null,
        uncertaintyMs: null,
        roundTripMs: null,
        osTimeZone: null,
        timeZone: null,
        logTimeZone: null,
      }
    }
    return {
      serverId: rec.id,
      reachable: true,
      clock: new Date(now + clock.offsetMs).toISOString(),
      differenceMs: rec.id === ANSWER ? 0 : clock.offsetMs,
      uncertaintyMs: rec.id === ANSWER ? 0 : clock.uncertaintyMs,
      roundTripMs: rec.id === ANSWER ? 0 : clock.roundTripMs,
      osTimeZone: clock.osTimeZone,
      timeZone: srv.run.TimeZone?.value ?? null,
      logTimeZone: srv.run.log_timezone?.value ?? null,
    }
  })
}

function clusterTime(c: Ctx): ReturnType<Handler> {
  const s = c.s
  const cfg = sync(s)
  return ok({
    measuredAt: new Date().toISOString(),
    referenceServerId: ANSWER,
    maxDifferenceMs: s.settings.clock?.maxDifferenceMs ?? 1000,
    servers: timeViews(s),
    databases: s.databases.map((d) => ({
      name: d.info.name,
      timeZone:
        cfg.overrides.find(
          (o) => o.level === 'database' && o.scopeId === d.info.name && o.name === 'TimeZone',
        )?.value ?? null,
    })),
  } satisfies S['ClusterTime'])
}

// --- restart ------------------------------------------------------------------------------------------------

function restart(c: Ctx): ReturnType<Handler> {
  const s = c.s
  const cfg = sync(s)
  const req = body<S['RestartPendingRequest']>(c)
  if (req.acknowledgeDowntime !== true)
    throw validationFailed([
      {
        field: '/acknowledgeDowntime',
        message:
          'Restarting a primary is downtime for its shard group, and failover is not available yet: confirm that you accept it.',
      },
    ])
  ensureNoConfigTask(s)
  const wanted =
    req.serverIds && req.serverIds.length > 0 ? req.serverIds : s.servers.map((x) => x.id)
  for (const id of wanted)
    if (!s.servers.some((x) => x.id === id))
      throw problem('not_found', `Server ${id} does not exist.`)
  const waiting = wanted.filter(
    (id) => Object.keys((cfg.servers[id] as ServerCfg).pending).length > 0,
  )
  if (waiting.length === 0) throw problem('nothing_pending', 'No instance waits for a restart.')
  const moves = waiting.flatMap((id) =>
    Object.entries((cfg.servers[id] as ServerCfg).pending).map(([name, after]) => ({
      name,
      before: (cfg.servers[id] as ServerCfg).run[name]?.value ?? null,
      after,
    })),
  )
  const dir = directions(cfg, moves)
  const conflict = conflictText(dir)
  if (conflict) throw problem('config_restart_order_conflict', conflict)
  const order = restartOrderOf(s, waiting, dir)
  return accepted(
    createTask(s, {
      kind: 'config.restart',
      subject: { type: 'config', id: 'restart' },
      steps: order.map(
        (o) => `Restart instance ${o.instanceId} (${o.role}) on server ${o.serverId}`,
      ),
      durationMs: 3000,
      apply: () => {
        for (const id of waiting) applyPending(s, id)
      },
    }),
  )
}

/** A restart of a server's instance starts a new postmaster that reads the file: what waited is now running. */
export function applyPending(s: MockState, serverId: string): void {
  const srv = s.config.servers[serverId]
  if (!srv) return
  for (const [name, value] of Object.entries(srv.pending)) {
    const w = wantedValue(s.config, serverId, name)
    srv.run[name] = { value, source: w.source }
    delete srv.pending[name]
  }
  srv.appliedRevision = s.config.revision
}

// --- switches and helpers for the tests -------------------------------------------------------------------

/** `?config=failed:<server>`: the next change fails on that server (a retry works). */
export function failNextApply(s: MockState, serverId: string, text?: string): void {
  s.config.inject.failServers.set(
    serverId,
    text ??
      `could not apply the configuration file "/var/lib/kingresdb/data/g1/kingres-config.conf": parameter value rejected by PostgreSQL`,
  )
}

/** `?config=waiting:<server>`: the server cannot be reached for a configuration change until `bringConfigServerBack`. */
export function takeConfigServerDown(s: MockState, serverId: string): void {
  s.config.inject.downServers.add(serverId)
}

export function bringConfigServerBack(s: MockState, serverId: string): void {
  const cfg = s.config
  cfg.inject.downServers.delete(serverId)
  const srv = cfg.servers[serverId]
  if (srv) {
    srv.waiting = false
    srv.waitDetail = null
  }
  const rec = s.tasks.find(
    (t) => t.task.kind === 'config.apply' && t.task.state === 'running' && t.holdAt !== undefined,
  )
  if (rec) {
    rec.holdAt = undefined
    rec.holdDetail = undefined
    rec.task.cancellable = false
  }
  if (!rec && srv) reconcile(cfg, serverId, undefined, [])
}

/** `?config=restart-conflict`: max_connections goes up and max_wal_senders goes down at the next restart. */
export function seedRestartConflict(s: MockState): void {
  const cfg = s.config
  cfg.inject.restartConflict = true
  cfg.revision += 1
  const stamp = (name: string, value: string): void => {
    const def = paramOf(cfg, name)
    if (!def) return
    cfg.overrides.push({
      level: 'cluster',
      scopeId: null,
      name: def.name,
      value,
      revision: cfg.revision,
      updatedBy: 'admin',
      updatedAt: new Date().toISOString(),
    })
    cfg.changes.push({
      changeId: cfg.nextChangeId++,
      revision: cfg.revision,
      name: def.name,
      level: 'cluster',
      scopeId: null,
      oldValue: null,
      newValue: value,
      changedBy: 'admin',
      changedAt: new Date().toISOString(),
      taskId: null,
      reverts: null,
    })
    for (const srv of Object.values(cfg.servers)) srv.pending[def.name] = value
  }
  stamp('max_connections', '200')
  stamp('max_wal_senders', '6')
}

/** `?clock=off:<server>:<ms>`. */
export function setClockOffset(s: MockState, serverId: string, ms: number): void {
  const clock = s.config.clocks[serverId]
  if (clock) {
    clock.offsetMs = ms
    clock.reachable = true
  }
}

/** `?clock=unreachable:<server>`. */
export function setClockReachable(s: MockState, serverId: string, reachable: boolean): void {
  const clock = s.config.clocks[serverId]
  if (clock) clock.reachable = reachable
}

// --- the table of handlers ----------------------------------------------------------------------------------

export function configHandlers(): Partial<Record<OperationId, Handler>> {
  return {
    listConfigParameters: listParameters,
    getConfigParameter: (c) => {
      const cfg = sync(c.s)
      const def = paramOf(cfg, c.params.name ?? '')
      if (!def)
        throw problem(
          'config_parameter_unknown',
          `The server has no parameter called "${c.params.name}".`,
        )
      return ok(parameterView(c.s, def))
    },
    previewConfigChange: preview,
    applyConfigChange: applyChange,
    listConfigChanges: listChanges,
    revertConfigChange: revert,
    listConfigServers: (c) => {
      const cfg = sync(c.s)
      return ok({ items: c.s.servers.map((x) => serverStatus(c.s, x.id)), revision: cfg.revision })
    },
    getConfigSummary: summary,
    getClusterTime: clusterTime,
    listTimeZones: (c) => {
      sync(c.s)
      return ok({ items: TIME_ZONES })
    },
    restartPendingInstances: restart,
  }
}
