// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The data side of the configuration feature of the mock (contract 0.8.0): the parameter catalogue with its policy
// (managed, refused, hidden, cluster-only, allowed levels), the overrides and their history, what each server runs and
// what waits for a restart, and the clocks. No logic of requests lives here; `config.ts` has it. This file imports no
// value from `state.ts`, so `state.ts` can create the configuration state without a cycle.
import type {
  ConfigLevel,
  ConfigParameterKind,
  ConfigValueSource,
  ConfigParameter,
} from '@/api/types'
import { formatBase } from '@/lib/config-value'
import { CATALOG, CATEGORY } from './config-catalog'

type Context = ConfigParameter['context']

export interface ParamDef {
  name: string
  category: string
  description: string
  type: ConfigParameter['type']
  unit: string | null
  min: string | null
  max: string | null
  enumValues: string[] | null
  context: Context
  /** The built-in default in the base unit (`boot_val`). */
  bootBase: string
  kind: ConfigParameterKind
  reason: string | null
  hidden: boolean
  allowedLevels: ConfigLevel[]
  clusterOnly: string | null
}

export interface OverrideRec {
  level: ConfigLevel
  scopeId: string | null
  name: string
  value: string
  revision: number
  updatedBy: string
  updatedAt: string
}

export interface ChangeRec {
  changeId: number
  revision: number
  name: string
  level: ConfigLevel
  scopeId: string | null
  oldValue: string | null
  newValue: string | null
  changedBy: string
  changedAt: string
  taskId: number | null
  reverts: number | null
}

export interface RunningValue {
  value: string
  source: ConfigValueSource
}

export interface ServerCfg {
  serverGroup: string | null
  appliedRevision: number
  /** What the instance runs now, for every parameter. */
  run: Record<string, RunningValue>
  /** Values that wait for a restart (`pending_restart`), by parameter. */
  pending: Record<string, string>
  /** PostgreSQL's text of a value that did not take, by parameter. */
  failed: Record<string, string>
  /** The server could not be reached or its instance does not run: it gets the change when it returns. */
  waiting: boolean
  waitDetail: string | null
}

export interface ClockRec {
  offsetMs: number
  uncertaintyMs: number
  roundTripMs: number
  osTimeZone: string | null
  reachable: boolean
}

export interface ConfigState {
  revision: number
  params: ParamDef[]
  byLower: Map<string, ParamDef>
  overrides: OverrideRec[]
  changes: ChangeRec[]
  nextChangeId: number
  servers: Record<string, ServerCfg>
  /** What a server runs before any override (the managed, initdb, derived and built-in values). */
  base: Record<string, Record<string, RunningValue>>
  clocks: Record<string, ClockRec>
  /** The switches `?config=...` and `?clock=...` and the helpers of the browser tests. */
  inject: {
    /** Servers whose next apply fails (once: a retry works). */
    failServers: Map<string, string>
    /** Servers that cannot be reached for a configuration change until `bringBack`. */
    downServers: Set<string>
    restartConflict: boolean
  }
}

// --- policy -----------------------------------------------------------------------------------

const CONTEXT: Record<string, Context> = {
  p: 'postmaster',
  s: 'sighup',
  S: 'superuser',
  u: 'user',
  b: 'backend',
  B: 'superuser-backend',
  i: 'internal',
}

const MANAGED: [string[], string][] = [
  [
    ['listen_addresses', 'port', 'unix_socket_directories'],
    'KingresDB needs the address and port that the other servers and kingresd connect to; a wrong value would cut the server off.',
  ],
  [
    ['shared_preload_libraries'],
    'KingresDB sets the extensions the cluster runs on (kingres_shard, kingres_ledger).',
  ],
  [
    [
      'ssl',
      'ssl_cert_file',
      'ssl_key_file',
      'ssl_ca_file',
      'ssl_crl_file',
      'ssl_min_protocol_version',
    ],
    'The certificates of the cluster and the TLS floor are managed by KingresDB.',
  ],
  [
    ['kingres_shard.node_conninfo'],
    'KingresDB writes the certificate options of the node connections.',
  ],
  [['password_encryption'], 'Verifiers are copied between servers and must be SCRAM.'],
  [
    ['logging_collector', 'log_directory', 'log_filename', 'log_truncate_on_rotation'],
    'The console reads the log files that KingresDB configures.',
  ],
  [
    [
      'log_min_error_statement',
      'log_statement',
      'log_min_duration_statement',
      'log_parameter_max_length',
      'log_parameter_max_length_on_error',
    ],
    'Statement text must never be written to a log.',
  ],
  [['cluster_name'], 'KingresDB names the instances (process titles, logs).'],
  [
    ['hot_standby', 'primary_conninfo', 'primary_slot_name'],
    'KingresDB maintains the connection of a standby to its primary.',
  ],
  [
    ['synchronous_standby_names'],
    'The primary of each shard group keeps this list from the standbys that stream.',
  ],
  [
    ['synchronous_commit'],
    'The durability of each database is set with ALTER DATABASE; a second writer would fight with it.',
  ],
  [
    ['wal_level'],
    'Replication needs at least replica and the bucket moves need logical; KingresDB owns the value.',
  ],
  [
    ['hba_file', 'ident_file', 'config_file', 'data_directory', 'external_pid_file'],
    'File locations are fixed by KingresDB; pg_hba.conf and pg_ident.conf are generated from the network rules.',
  ],
]

const REFUSED: [string[], string][] = [
  [
    [
      'archive_command',
      'archive_cleanup_command',
      'restore_command',
      'recovery_end_command',
      'ssl_passphrase_command',
    ],
    'Runs a command as the operating system user of the database. Set it on the server, in postgresql.conf.',
  ],
  [
    [
      'archive_library',
      'session_preload_libraries',
      'local_preload_libraries',
      'dynamic_library_path',
      'extension_control_path',
      'jit_provider',
      'oauth_validator_libraries',
    ],
    'Loads code into the database. Set it on the server, in postgresql.conf.',
  ],
  [
    ['krb_server_keyfile', 'ssl_dh_params_file', 'ssl_crl_dir', 'unix_socket_group'],
    'Names a file or an account of the operating system. Set it on the server, in postgresql.conf.',
  ],
  [
    [
      'ssl_ciphers',
      'ssl_groups',
      'ssl_max_protocol_version',
      'ssl_tls13_ciphers',
      'ssl_ecdh_curve',
    ],
    'Part of the TLS policy of the cluster; a wrong value can lock every server out. Set it on the server, in postgresql.conf.',
  ],
  [
    ['fsync', 'full_page_writes'],
    'With this off a crash can corrupt a server that the cluster believes holds acknowledged commits.',
  ],
  [
    ['kingres_shard.fault_injection', 'kingres_shard.forwarded', 'kingres_shard.forward_token'],
    'A test hook or a per-session internal of the sharding extension.',
  ],
]

/** Superuser-only parameters (`GUC_SUPERUSER_ONLY`): PostgreSQL hides their values from an ordinary database user. */
const HIDDEN = new Set([
  'archive_command',
  'archive_library',
  'archive_cleanup_command',
  'restore_command',
  'recovery_end_command',
  'ssl_passphrase_command',
  'dynamic_library_path',
  'extension_control_path',
  'local_preload_libraries',
  'session_preload_libraries',
  'shared_preload_libraries',
  'primary_conninfo',
  'data_directory',
  'hba_file',
  'ident_file',
  'config_file',
  'external_pid_file',
  'log_directory',
  'ssl_cert_file',
  'ssl_key_file',
  'ssl_ca_file',
  'ssl_crl_file',
  'ssl_crl_dir',
  'ssl_dh_params_file',
  'krb_server_keyfile',
  'unix_socket_directories',
  'kingres_shard.node_conninfo',
  'kingres_shard.forward_token',
  'debug_io_direct',
  'wal_consistency_checking',
])

const CLUSTER_ONLY: [string[], string][] = [
  [
    ['TimeZone', 'timezone_abbreviations', 'log_timezone'],
    'A function evaluated on another node (now()::timestamp, date_trunc, current_date) gives another result when the zone differs, and the logs of the cluster should read as one.',
  ],
  [
    [
      'lc_monetary',
      'lc_numeric',
      'lc_time',
      'default_text_search_config',
      'DateStyle',
      'IntervalStyle',
      'extra_float_digits',
      'standard_conforming_strings',
      'bytea_output',
      'default_transaction_isolation',
      'default_transaction_read_only',
      'default_transaction_deferrable',
    ],
    'This setting must be equal on every server: it changes the result of a statement or the default a transaction takes on the node it lands on.',
  ],
  [
    [
      'kingres_shard.default_bucket_count',
      'kingres_shard.enable_routing',
      'kingres_shard.enable_distributed_scan',
    ],
    'It decides how a statement is routed; a node that routes differently returns different rows.',
  ],
  [
    ['kingres_ledger.block_interval', 'kingres_ledger.max_block_transactions'],
    'The ledger chain is cluster-wide; blocks closed by different rules make a chain that is hard to verify.',
  ],
  [
    ['max_prepared_transactions'],
    'Two-phase commit across nodes needs a usable value on every node.',
  ],
]

/** The values `kingresd` writes itself (SHOW form). */
const MANAGED_VALUES: Record<string, string> = {
  listen_addresses: '*',
  port: '5432',
  ssl: 'on',
  ssl_min_protocol_version: 'TLSv1.2',
  password_encryption: 'scram-sha-256',
  logging_collector: 'on',
  log_filename: 'kingres-%Y-%m-%d.log',
  log_truncate_on_rotation: 'on',
  log_min_error_statement: 'panic',
  log_statement: 'none',
  log_min_duration_statement: '-1',
  log_parameter_max_length: '0',
  log_parameter_max_length_on_error: '0',
  cluster_name: 'prod-eu',
  hot_standby: 'on',
  synchronous_commit: 'on',
  wal_level: 'logical',
}

/** What `initdb` wrote into postgresql.conf from the machine's locale and zone. */
const INITDB_VALUES: Record<string, string> = {
  TimeZone: 'Asia/Tehran',
  log_timezone: 'Asia/Tehran',
  lc_messages: 'en_US.UTF-8',
  lc_monetary: 'en_US.UTF-8',
  lc_numeric: 'en_US.UTF-8',
  lc_time: 'en_US.UTF-8',
  default_text_search_config: 'pg_catalog.english',
  DateStyle: 'ISO, MDY',
  client_encoding: 'UTF8',
}

/** Chosen by kingresd from the memory quota and the default connection count. */
const DERIVED_VALUES: Record<string, string> = {
  shared_buffers: '128MB',
  effective_cache_size: '512MB',
  maintenance_work_mem: '64MB',
  max_connections: '100',
  max_prepared_transactions: '16',
}

// --- the catalogue ------------------------------------------------------------------------------

function buildParams(): ParamDef[] {
  const reasonOf = (table: [string[], string][], name: string): string | null =>
    table.find(([names]) => names.includes(name))?.[1] ?? null
  const out: ParamDef[] = []
  for (const raw of CATALOG.split('\n')) {
    const line = raw.trim()
    if (!line) continue
    const [name, cat, t, unit, lo, hi, boot, ctx, ...desc] = line
      .split('|')
      .map((x) => x.trim()) as [
      string,
      string,
      string,
      string,
      string,
      string,
      string,
      string,
      ...string[],
    ]
    const type =
      t === 'b'
        ? 'bool'
        : t === 'i'
          ? 'integer'
          : t === 'r'
            ? 'real'
            : t === 'e'
              ? 'enum'
              : 'string'
    const context = CONTEXT[ctx] ?? 'user'
    const enumValues = type === 'enum' ? lo.split(',') : null
    const managedReason = reasonOf(MANAGED, name)
    const refusedReason = reasonOf(REFUSED, name)
    const category = CATEGORY[cat] ?? cat
    const hidden = HIDDEN.has(name)
    let kind: ConfigParameterKind = 'settable'
    let reason: string | null = null
    if (managedReason) {
      kind = 'managed'
      reason = managedReason
    } else if (refusedReason) {
      kind = 'refused'
      reason = refusedReason
    } else if (hidden) {
      kind = 'refused'
      reason = 'PostgreSQL hides this setting from ordinary database users: it may hold a secret.'
    } else if (category === 'Developer Options') {
      kind = 'refused'
      reason = 'A developer option: it exists to break safety checks.'
    } else if (context === 'internal') {
      kind = 'refused'
      reason = 'This setting is fixed when the server is built or initialized.'
    }
    const clusterOnly = kind === 'settable' ? reasonOf(CLUSTER_ONLY, name) : null
    let allowedLevels: ConfigLevel[] = []
    if (kind === 'settable') {
      const db = context === 'user' || context === 'superuser'
      allowedLevels = clusterOnly
        ? db
          ? ['cluster', 'database']
          : ['cluster']
        : db
          ? ['cluster', 'serverGroup', 'server', 'database']
          : ['cluster', 'serverGroup', 'server']
    }
    out.push({
      name,
      category,
      description: desc.join('|'),
      type,
      unit: unit === '-' ? null : unit,
      min: type === 'integer' || type === 'real' ? lo || null : null,
      max: type === 'integer' || type === 'real' ? hi || null : null,
      enumValues,
      context,
      bootBase: boot,
      kind,
      reason,
      hidden,
      allowedLevels,
      clusterOnly,
    })
  }
  // PostgreSQL sorts by name ignoring case, and `TimeZone` sorts among the t's.
  return out.sort((a, b) => {
    const x = a.name.toLowerCase()
    const y = b.name.toLowerCase()
    return x < y ? -1 : x > y ? 1 : 0
  })
}

/** The built-in default as `SHOW` writes it. */
export function bootText(p: ParamDef): string {
  return p.type === 'integer' ? formatBase(p.bootBase, p.unit) : p.bootBase
}

const lower = (name: string): string => name.toLowerCase()

/** What one server runs before any override. */
function baseFor(params: ParamDef[], serverId: string): Record<string, RunningValue> {
  const out: Record<string, RunningValue> = {}
  for (const p of params) {
    const managed = MANAGED_VALUES[p.name]
    const initdb = INITDB_VALUES[p.name]
    const derived = DERIVED_VALUES[p.name]
    out[p.name] =
      managed !== undefined
        ? { value: managed, source: 'managed' }
        : initdb !== undefined
          ? { value: initdb, source: 'initdb' }
          : derived !== undefined
            ? { value: derived, source: 'derived' }
            : { value: bootText(p), source: 'default' }
  }
  // Somebody edited postgresql.conf of this one server by hand: no override explains it.
  if (serverId === '10.0.0.12') out.checkpoint_timeout = { value: '10min', source: 'other' }
  return out
}

/**
 * The value the stored overrides want on a server: the server level beats the server group beats the cluster beats
 * the base. `extra` are overrides that are not stored yet (the preview).
 */
export function wantedValue(
  cfg: ConfigState,
  serverId: string,
  name: string,
  overrides: OverrideRec[] = cfg.overrides,
): { value: string; source: ConfigValueSource; level: ConfigLevel | null } {
  const group = cfg.servers[serverId]?.serverGroup ?? null
  const mine = overrides.filter((o) => lower(o.name) === lower(name))
  const server = mine.find((o) => o.level === 'server' && o.scopeId === serverId)
  const grp =
    group === null ? undefined : mine.find((o) => o.level === 'serverGroup' && o.scopeId === group)
  const cluster = mine.find((o) => o.level === 'cluster')
  const hit = server ?? grp ?? cluster
  if (hit) return { value: hit.value, source: hit.level as ConfigValueSource, level: hit.level }
  const base = cfg.base[serverId]?.[cfg.byLower.get(lower(name))?.name ?? name]
  return { value: base?.value ?? '', source: base?.source ?? 'default', level: null }
}

const ago = (seconds: number): string => new Date(Date.now() - seconds * 1000).toISOString()

/** The configuration of the mock cluster: revision 6, one value waiting for a restart, one time zone per database. */
export function createConfigState(
  servers: { id: string; serverGroup: string | null }[],
): ConfigState {
  const params = buildParams()
  const byLower = new Map(params.map((p) => [lower(p.name), p]))
  const cfg: ConfigState = {
    revision: 6,
    params,
    byLower,
    overrides: [],
    changes: [],
    nextChangeId: 7,
    servers: {},
    base: {},
    clocks: {},
    inject: { failServers: new Map(), downServers: new Set(), restartConflict: false },
  }
  const stamp = (
    revision: number,
    seconds: number,
    name: string,
    level: ConfigLevel,
    scopeId: string | null,
    oldValue: string | null,
    newValue: string | null,
    by: string,
    id: number,
  ): void => {
    cfg.changes.push({
      changeId: id,
      revision,
      name,
      level,
      scopeId,
      oldValue,
      newValue,
      changedBy: by,
      changedAt: ago(seconds),
      taskId: null,
      reverts: null,
    })
    const existing = cfg.overrides.findIndex(
      (o) => lower(o.name) === lower(name) && o.level === level && o.scopeId === scopeId,
    )
    if (existing >= 0) cfg.overrides.splice(existing, 1)
    if (newValue !== null)
      cfg.overrides.push({
        level,
        scopeId,
        name,
        value: newValue,
        revision,
        updatedBy: by,
        updatedAt: ago(seconds),
      })
  }
  stamp(1, 86400 * 9, 'shared_buffers', 'cluster', null, null, '256MB', 'admin', 1)
  stamp(2, 86400 * 7, 'work_mem', 'cluster', null, null, '16MB', 'admin', 2)
  stamp(3, 86400 * 5, 'effective_io_concurrency', 'serverGroup', 'rack-a', null, '200', 'ops', 3)
  stamp(4, 86400 * 3, 'work_mem', 'server', '10.0.0.13', null, '32MB', 'admin', 4)
  stamp(5, 86400 * 2, 'TimeZone', 'database', 'reports', null, 'Europe/Berlin', 'admin', 5)
  stamp(6, 3600 * 5, 'work_mem', 'cluster', null, '16MB', '8MB', 'admin', 6)
  for (const s of servers) {
    cfg.base[s.id] = baseFor(params, s.id)
    cfg.servers[s.id] = {
      serverGroup: s.serverGroup,
      appliedRevision: 6,
      run: {},
      pending: {},
      failed: {},
      waiting: false,
      waitDetail: null,
    }
  }
  for (const s of servers) {
    const srv = cfg.servers[s.id] as ServerCfg
    for (const p of params) {
      const w = wantedValue(cfg, s.id, p.name)
      srv.run[p.name] = { value: w.value, source: w.source }
    }
  }
  // The restart that applies shared_buffers has been done on two servers; the third one still runs the old value.
  const waiting = cfg.servers['10.0.0.12']
  if (waiting) {
    waiting.run.shared_buffers = { value: '128MB', source: 'derived' }
    waiting.pending.shared_buffers = '256MB'
  }
  cfg.clocks = {
    '10.0.0.11': {
      offsetMs: 0,
      uncertaintyMs: 0,
      roundTripMs: 0,
      osTimeZone: 'Asia/Tehran',
      reachable: true,
    },
    '10.0.0.12': {
      offsetMs: 0.3,
      uncertaintyMs: 0.4,
      roundTripMs: 0.6,
      osTimeZone: 'Asia/Tehran',
      reachable: true,
    },
    '10.0.0.13': {
      offsetMs: 1820,
      uncertaintyMs: 0.4,
      roundTripMs: 0.6,
      osTimeZone: 'UTC',
      reachable: true,
    },
  }
  return cfg
}

export { lower as lowerName }

/** Adds a parameter to the catalogue (the hostile data of the tests and of `?mock=hostile`). */
export function addParameter(
  cfg: ConfigState,
  def: ParamDef,
  values: Record<string, string>,
): void {
  cfg.params.push(def)
  cfg.params.sort((a, b) => {
    const x = a.name.toLowerCase()
    const y = b.name.toLowerCase()
    return x < y ? -1 : x > y ? 1 : 0
  })
  cfg.byLower.set(lower(def.name), def)
  for (const [id, srv] of Object.entries(cfg.servers)) {
    const value = values[id] ?? bootText(def)
    srv.run[def.name] = { value, source: 'default' }
    ;(cfg.base[id] ??= {})[def.name] = { value, source: 'default' }
  }
}
