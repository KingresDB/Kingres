// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Hostile data for the mock: names and messages as a low-privileged database user could make them
// (anyone who may create a role, a table or a column chooses its name; anyone can make the server
// log a line). Used by the tests and by `?mock=hostile` in development, never in a build.
import type { MissingServer } from '@/api/types'
import type { MockState } from './state'
import { mockConfig, nowIso } from './state'
import { addParameter, type ParamDef } from './config-data'

const cp = (...codes: number[]) => String.fromCodePoint(...codes)

/** One string per trick. */
export const HOSTILE = {
  markup: '<img src=x onerror=window.__pwn=1>',
  breakout: '"><svg onload=window.__pwn=1>',
  reversed: `${cp(0x202e)}nimda`,
  zeroWidth: `admin${cp(0x200b)}`,
  cyrillic: `${cp(0x0430)}dmin`,
  mixedScript: `sh${cp(0x043e)}${cp(0x0440)}`,
  isolate: `${cp(0x2066)}abc${cp(0x2069)}`,
  controls: `a${cp(0x1b)}[31mb${cp(0)}${cp(7)}c`,
  newline: 'first line\n2026-11-14 12:00:00 ERROR fake second log line',
  slashy: 'a/b?c#d%e',
  long: 'L'.repeat(5000),
} as const

/** Names that can be created as roles, databases and tables. */
export const HOSTILE_NAMES: string[] = [
  HOSTILE.markup,
  HOSTILE.breakout,
  HOSTILE.reversed,
  HOSTILE.zeroWidth,
  HOSTILE.cyrillic,
  HOSTILE.mixedScript,
  HOSTILE.isolate,
  HOSTILE.slashy,
  HOSTILE.long,
]

/** Databases that get a copy of `shop` under a hostile name. `..` cannot be addressed by URL. */
export const HOSTILE_DATABASES = [
  HOSTILE.markup,
  HOSTILE.reversed,
  `shop${cp(0x200b)}`,
  `sh${cp(0x043e)}${cp(0x0440)}`,
  HOSTILE.slashy,
  HOSTILE.long,
  '..',
]

/**
 * Servers that did not answer a cluster-wide read, with ids chosen by someone who controls a host name
 * (contract 0.5.0: `missingServers`). `?mock=hostile` switches the mock to a partial answer with these.
 */
export const HOSTILE_MISSING: MissingServer[] = [
  { serverId: HOSTILE.reversed, reason: 'timeout' },
  { serverId: HOSTILE.markup, reason: 'unreachable' },
  { serverId: HOSTILE.zeroWidth, reason: 'refused' },
  { serverId: HOSTILE.long, reason: 'invalidAnswer' },
]

/** Adds the hostile data to a fresh mock state. */
export function seedHostile(s: MockState): void {
  const template = s.roles[0]
  if (!template) return
  // Texts a hostile server could send in the fields added in 0.6.0: the setup task `detail` and the
  // fingerprints it reports about itself (shown through SafeName).
  mockConfig.hostileSetupText = HOSTILE.markup

  for (const name of HOSTILE_NAMES) {
    s.roles.push({
      ...template,
      name,
      isSuperuser: false,
      clusterRoles: [],
      memberOf: [],
      canLogin: true,
    })
  }
  // A role that is a member of hostile roles, to put hostile names into "Member of".
  s.roles.push({
    ...template,
    name: 'app_member',
    isSuperuser: false,
    clusterRoles: [],
    memberOf: [HOSTILE.reversed, HOSTILE.markup],
  })

  const shop = s.databases.find((d) => d.info.name === 'shop')
  if (shop) {
    for (const name of HOSTILE_DATABASES) {
      const copy = structuredClone(shop)
      copy.info.name = name
      s.databases.push(copy)
    }
    // Hostile tables, schemas and columns in `shop` itself.
    const plain = shop.tables.find((t) => t.classes.includes('plain'))
    const distributed = shop.tables.find((t) => t.classes.includes('distributed'))
    if (plain && distributed) {
      const tables: [string, string, boolean][] = [
        ['public', HOSTILE.markup, false],
        ['public', HOSTILE.reversed, false],
        ['public', `orders${cp(0x200b)}`, false],
        ['public', HOSTILE.long, false],
        ['public', HOSTILE.slashy, false],
        [HOSTILE.breakout, 'events', false],
        // Two distributed tables whose "schema.table" text is the same string.
        ['x', 'y.z', true],
        ['x.y', 'z', true],
        ['public', '..', false],
      ]
      shop.extraColumns = {}
      for (const [schema, name, isDistributed] of tables) {
        shop.tables.push({ ...structuredClone(isDistributed ? distributed : plain), schema, name })
        shop.extraColumns[name] = [
          {
            name: 'id',
            type: 'bigint',
            nullable: false,
            blockingKeys: [],
            canBeDistributionColumn: true,
            distributionBlockedReason: null,
          },
          {
            name: HOSTILE.markup,
            type: 'text',
            nullable: true,
            blockingKeys: [],
            canBeDistributionColumn: false,
            distributionBlockedReason: 'notInUniqueKey',
          },
          {
            name: HOSTILE.reversed,
            type: 'text',
            nullable: true,
            blockingKeys: [HOSTILE.reversed],
            canBeDistributionColumn: false,
            distributionBlockedReason: 'notInUniqueKey',
          },
        ]
      }
    }
  }

  // Hostile schema names (contract 0.15.0): anyone who may create a schema chooses its name. Every other one is
  // distributed; one local schema has blockers whose object names and reasons are hostile too.
  const tenants = s.databases.find((d) => d.info.name === 'tenants')
  if (tenants?.schemas) {
    const local = tenants.schemas.find((x) => x.group === null && x.blockers.length === 0)
    if (local) {
      HOSTILE_NAMES.forEach((name, i) => {
        tenants.schemas?.push({
          ...structuredClone(local),
          name,
          group: i % 2 === 0 ? (i % 3) + 1 : null,
          colocationId: 100 + i,
          sizeBytes: i % 3 === 0 ? null : 1024 * (i + 1),
        })
      })
      tenants.schemas.push({
        ...structuredClone(local),
        name: `blocked${cp(0x202e)}`,
        blockers: [
          { objectKind: HOSTILE.markup, objectName: HOSTILE.breakout, reason: HOSTILE.reversed },
          { objectKind: 'table', objectName: HOSTILE.zeroWidth, reason: HOSTILE.newline },
        ],
      })
    }
  }

  s.serverGroups.push(
    { name: HOSTILE.markup, description: HOSTILE.newline },
    { name: HOSTILE.reversed, description: HOSTILE.controls },
  )
  const first = s.servers[0]
  if (first) first.hostname = HOSTILE.reversed

  const base = { kind: 'serverDown' as const, serverId: null, raisedAt: nowIso() }
  s.alerts.unshift(
    {
      ...base,
      id: 900,
      severity: 'critical',
      state: 'open',
      title: HOSTILE.markup,
      message: HOSTILE.newline,
      databaseName: HOSTILE.reversed,
    },
    {
      ...base,
      id: 901,
      severity: 'warning',
      state: 'acknowledged',
      title: HOSTILE.reversed,
      message: HOSTILE.long,
      databaseName: null,
      acknowledgedAt: nowIso(),
      acknowledgedBy: HOSTILE.reversed,
    },
    // Alerts whose subject is a server with a hostile id (contract 0.5.0: certificateRenewalFailed).
    {
      ...base,
      id: 902,
      kind: 'certificateRenewalFailed',
      severity: 'warning',
      state: 'open',
      title: HOSTILE.breakout,
      message: HOSTILE.controls,
      serverId: HOSTILE.reversed,
      databaseName: null,
    },
    {
      ...base,
      id: 903,
      kind: 'certificateRenewalFailed',
      severity: 'warning',
      state: 'open',
      title: HOSTILE.cyrillic,
      message: HOSTILE.newline,
      serverId: '..',
      databaseName: null,
    },
    // 0.7.0: the typed subject of an alert (server ids, database name and qualifier chosen by whoever controls them).
    {
      ...base,
      id: 904,
      kind: 'replicationPending',
      severity: 'warning',
      state: 'open',
      title: HOSTILE.markup,
      message: HOSTILE.newline,
      serverId: HOSTILE.reversed,
      databaseName: null,
      subject: {
        serverId: HOSTILE.reversed,
        behindServerId: HOSTILE.zeroWidth,
        shardGroupId: null,
        databaseName: null,
        aspect: null,
      },
    },
    {
      ...base,
      id: 905,
      kind: 'ledgerPendingTooOld',
      severity: 'warning',
      state: 'open',
      title: HOSTILE.cyrillic,
      message: HOSTILE.controls,
      serverId: '..',
      databaseName: HOSTILE.markup,
      subject: {
        serverId: '..',
        behindServerId: null,
        shardGroupId: 2,
        databaseName: HOSTILE.markup,
        aspect: HOSTILE.breakout,
      },
    },
  )

  const logs: string[] = [
    HOSTILE.markup,
    HOSTILE.breakout,
    HOSTILE.reversed,
    HOSTILE.controls,
    HOSTILE.newline,
    HOSTILE.long,
  ]
  for (const message of logs) {
    s.logs.unshift({
      timestamp: nowIso(),
      level: 'error',
      component: 'postgres',
      serverId: '10.0.0.11',
      instanceId: null,
      message,
    })
  }

  let id = s.nextAuditId
  for (const name of [HOSTILE.reversed, HOSTILE.markup, HOSTILE.cyrillic]) {
    s.audit.unshift({
      id: id++,
      timestamp: nowIso(),
      actor: name,
      source: 'panel',
      action: 'createRole',
      resourceType: 'role',
      resourceId: name,
      outcome: 'success',
      actorAddress: '10.0.0.77',
      serverId: HOSTILE.reversed,
    })
  }
  s.nextAuditId = id

  s.network.rules.push({
    type: 'host',
    databases: [HOSTILE.reversed],
    users: [HOSTILE.markup],
    address: '10.9.0.0/16',
    method: 'reject',
    comment: HOSTILE.newline,
  })

  // A failed task with a hostile subject and error message.
  const now = nowIso()
  s.tasks.unshift({
    task: {
      id: s.nextTaskId++,
      kind: 'database.drop',
      state: 'failed',
      progress: {
        percent: 40,
        currentStep: null,
        steps: [{ name: HOSTILE.markup, state: 'failed' }],
      },
      error: { code: 'x', message: HOSTILE.reversed, step: HOSTILE.breakout },
      detail: HOSTILE.markup,
      subject: { type: 'database', id: HOSTILE.reversed },
      cancellable: false,
      createdAt: now,
      updatedAt: now,
      createdBy: HOSTILE.reversed,
    },
    startedAt: Date.now(),
    durationMs: 1,
    apply: () => undefined,
    applied: true,
    setup: false,
  })

  seedHostileConfig(s)
  seedHostileLedger(s)

  s.extraQueryTables = {
    hostile: {
      columns: [
        { name: HOSTILE.markup, type: 'text' },
        { name: HOSTILE.reversed, type: 'text' },
        { name: 'dup', type: 'int4' },
        { name: 'dup', type: 'text' },
      ],
      rows: [
        [HOSTILE.markup, HOSTILE.reversed, 1, 'NULL'],
        [null, '', 2, HOSTILE.controls],
        [HOSTILE.newline, HOSTILE.long, 3, HOSTILE.cyrillic],
      ],
    },
  }
}

/**
 * Parameters as a low-privileged database user could make them: a placeholder setting of an extension has a name,
 * a category and a value that its role chose, and PostgreSQL's error texts quote the value. Names, categories,
 * descriptions, enum values, values on the servers and at every level, and error texts are all hostile.
 */
function seedHostileConfig(s: MockState): void {
  const cfg = s.config
  const common = {
    unit: null,
    min: null,
    max: null,
    enumValues: null,
    context: 'user' as const,
    bootBase: HOSTILE.reversed,
    kind: 'settable' as const,
    reason: null,
    hidden: false,
    allowedLevels: ['cluster', 'serverGroup', 'server', 'database'] as ParamDef['allowedLevels'],
    clusterOnly: null,
  }
  const text: ParamDef = {
    ...common,
    name: HOSTILE.markup,
    category: HOSTILE.reversed,
    description: HOSTILE.newline,
    type: 'string',
  }
  const mode: ParamDef = {
    ...common,
    name: `hostile.${HOSTILE.breakout}`,
    category: HOSTILE.markup,
    description: HOSTILE.controls,
    type: 'enum',
    enumValues: [HOSTILE.markup, HOSTILE.reversed, HOSTILE.zeroWidth, 'plain'],
    bootBase: 'plain',
  }
  const ids = s.servers.map((x) => x.id)
  addParameter(cfg, text, {
    [ids[0] ?? '']: HOSTILE.markup,
    [ids[1] ?? '']: HOSTILE.long,
    [ids[2] ?? '']: HOSTILE.cyrillic,
  })
  addParameter(cfg, mode, { [ids[1] ?? '']: HOSTILE.reversed })
  const stamp = (
    level: 'cluster' | 'serverGroup' | 'server' | 'database',
    scopeId: string | null,
    value: string,
  ) => {
    cfg.overrides.push({
      level,
      scopeId,
      name: text.name,
      value,
      revision: cfg.revision,
      updatedBy: HOSTILE.reversed,
      updatedAt: nowIso(),
    })
    cfg.changes.push({
      changeId: cfg.nextChangeId++,
      revision: cfg.revision,
      name: text.name,
      level,
      scopeId,
      oldValue: HOSTILE.long,
      newValue: value,
      changedBy: HOSTILE.reversed,
      changedAt: nowIso(),
      taskId: null,
      reverts: null,
    })
  }
  stamp('cluster', null, HOSTILE.long)
  stamp('serverGroup', HOSTILE.markup, HOSTILE.reversed)
  stamp('database', HOSTILE.reversed, HOSTILE.breakout)
  // A failure whose text quotes the value that PostgreSQL rejected.
  const second = cfg.servers[ids[1] ?? '']
  if (second) {
    second.failed[text.name] =
      `invalid value for parameter "${HOSTILE.markup}": "${HOSTILE.reversed}"
${HOSTILE.long}`
  }
}

/**
 * The ledger of phase 7 as a hostile server could report it: the origin prefix, the public URL, the listener, the servers that
 * hold the key, the text of an alarm, the type of an event, a flag of an aggregate and its statement are all text with
 * markup, bidirectional controls and very long lines (contract 0.11.0). Fingerprints and public keys stay what the contract
 * says they are, but the identifier of the cluster is not trusted to be hexadecimal either.
 */
function seedHostileLedger(s: MockState): void {
  const l = s.ledger7
  l.hostileText = HOSTILE.markup
  l.settings.publication.originPrefix = `${HOSTILE.reversed}/x`
  l.settings.publication.publicUrl = `https://example.com/${HOSTILE.breakout}`
  l.publication.listener.address = HOSTILE.markup
  l.signing.holders = [HOSTILE.reversed, HOSTILE.markup, '10.0.0.12']
  l.signing.clusterId = HOSTILE.breakout
  l.signing.signerAlarm = HOSTILE.newline
  l.signing.events.push({ seq: l.signing.events.length, type: HOSTILE.markup, time: nowIso() })
  l.signing.events.push({ seq: l.signing.events.length, type: HOSTILE.reversed, time: nowIso() })
  // Anchoring (console part B): targets that are turned off (so the state of the ledger stays what it was) with names, a
  // configuration and a test result written by someone hostile, and records of a deleted target with a hostile name and error.
  for (const [i, name] of [HOSTILE.markup, HOSTILE.reversed, HOSTILE.long.slice(0, 63)].entries()) {
    l.targets.push({
      id: l.nextTargetId++,
      kind: (['worm', 'tsa', 'ots'] as const)[i] ?? 'worm',
      name,
      enabled: false,
      config:
        i === 0
          ? {
              endpoint: HOSTILE.breakout,
              bucket: HOSTILE.markup,
              region: 'x',
              mode: HOSTILE.reversed,
            }
          : i === 1
            ? { url: HOSTILE.breakout, caBundlePath: HOSTILE.markup }
            : { calendars: [HOSTILE.markup, HOSTILE.reversed] },
      credentialsSet: true,
      lastTest: {
        ok: false,
        testedAt: nowIso(),
        checks: [
          { name: HOSTILE.reversed, status: 'failed', detail: HOSTILE.newline },
          { name: HOSTILE.markup, status: 'warning', detail: HOSTILE.long },
        ],
      },
      newestAnchoredAt: null,
      state: 'none',
    })
  }
  // Client keys (console part C): a label, a role, a registrar and a reason written by someone hostile.
  l.clientKeys.shop = [HOSTILE.markup, HOSTILE.reversed, HOSTILE.long.slice(0, 100)].map(
    (label, i) => ({
      keyId: `0000000${i}`,
      publicKey: btoa(String.fromCharCode(...new Uint8Array(32).fill(65 + i))),
      roleName: i === 0 ? HOSTILE.zeroWidth : null,
      label,
      registeredBy: HOSTILE.reversed,
      registeredAt: nowIso(),
      revokedAt: i === 2 ? nowIso() : null,
      revocationReason: i === 2 ? HOSTILE.newline : null,
    }),
  )
  l.anchors.push({
    databaseName: 'shop',
    id: l.nextAnchorId++,
    targetId: null,
    targetName: HOSTILE.reversed,
    kind: 'worm',
    aggregateSize: 6,
    state: 'failed',
    attempts: 3,
    nextAttemptAt: null,
    anchoredAt: null,
    evidenceTime: null,
    lastError: HOSTILE.newline + HOSTILE.long,
  })
}
