// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Hostile data for the mock: names and messages as a low-privileged database user could make them
// (anyone who may create a role, a table or a column chooses its name; anyone can make the server
// log a line). Used by the tests and by `?mock=hostile` in development, never in a build.
import type { MockState } from './state'
import { nowIso } from './state'

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

/** Adds the hostile data to a fresh mock state. */
export function seedHostile(s: MockState): void {
  const template = s.roles[0]
  if (!template) return

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
