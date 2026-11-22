// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.11.0 (the ledger of phase 7: signing, publication, anchoring): every new alert kind, audit action, task kind,
// task subject and problem code has words, the mock answers each of the 28 new operations with a body that satisfies
// the schema of the contract and refuses what the contract says it refuses, and `?phase7=today` is the real server of
// today (seven operations work, the other 21 answer 422 `unsupported_operation`).
import { beforeEach, describe, expect, it } from 'vitest'
import { createPinia } from 'pinia'
import { waitFor } from '@testing-library/vue'
import { endpoints } from '@/api/endpoints'
import { ApiError, describeProblem } from '@/api/problem'
import { auditActions, operations } from '@/api/operations.generated'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { seedHostile } from '@/mocks/hostile'
import { NOT_BUILT_YET } from '@/mocks/ledger-data'
import {
  ALERT_KIND_LABEL,
  alertKindHint,
  alertKindLabel,
  AUDIT_ACTION_LABEL,
  auditActionLabel,
  TASK_KIND_LABEL,
  taskKindLabel,
  taskSubjectText,
} from '@/lib/status'
import type { ProblemCode } from '@/api/types'
import { screen } from '@testing-library/vue'
import { renderApp, signInAs } from './helpers'
import { ok, spec } from './schema-check'

const NEW_ALERT_KINDS = [
  'ledgerNotAnchored',
  'ledgerAnchorBehind',
  'ledgerAnchorFailed',
  'ledgerAnchorConflict',
  'ledgerChainDiverged',
  'ledgerGroupUnread',
  'ledgerPublicationBehind',
  'ledgerSignerUnavailable',
  'ledgerDigestOverdue',
] as const
const NEW_ACTIONS = [
  'updateLedgerSettings',
  'createLedgerSigningKey',
  'retireLedgerSigningKey',
  'revokeLedgerSigningKey',
  'createLedgerAnchorTarget',
  'updateLedgerAnchorTarget',
  'deleteLedgerAnchorTarget',
  'testLedgerAnchorTarget',
  'anchorLedgerNow',
  'retryLedgerAnchor',
  'createLedgerBundle',
  'registerLedgerClientKey',
  'revokeLedgerClientKey',
  'setTableSignaturePolicy',
  'upgradeLedgerExtension',
  'acknowledgeLedgerDivergence',
] as const
const NEW_TASK_KINDS = [
  'ledger.key.create',
  'ledger.key.rotate',
  'ledger.key.retire',
  'ledger.key.revoke',
  'ledger.key.distribute',
  'ledger.bundle.create',
  'ledger.epoch.start',
  'ledger.upgrade',
] as const
const NEW_CODES = [
  'ledger_not_published',
  'origin_prefix_locked',
  'signing_key_missing',
  'last_signing_key',
  'chain_not_diverged',
] as const
/** The seven operations that `kingresd` implements with revision 0.11.0. */
const BUILT = [
  'getLedgerSettings',
  'updateLedgerSettings',
  'getLedgerSigning',
  'createLedgerSigningKey',
  'retireLedgerSigningKey',
  'revokeLedgerSigningKey',
  'getLedgerTrustFile',
]

const schemas = spec.components.schemas as unknown as Record<string, { enum?: string[] }>

describe('words of 0.11.0', () => {
  it('every alert kind of the contract has words, and the nine new ones are in the contract', () => {
    const kinds = schemas.AlertKind?.enum as string[]
    for (const k of NEW_ALERT_KINDS) expect(kinds).toContain(k)
    for (const k of kinds) expect(alertKindLabel(k), k).not.toBe(k)
    for (const k of NEW_ALERT_KINDS) {
      expect(ALERT_KIND_LABEL[k]).toBeTruthy()
      // A sentence says what the alert asks of the operator.
      expect(alertKindHint(k), k).toBeTruthy()
    }
  })

  it('every audit action of the contract has words, and the sixteen new ones are in the contract', () => {
    for (const a of NEW_ACTIONS) {
      expect(auditActions).toContain(a)
      expect(AUDIT_ACTION_LABEL[a]).toBeTruthy()
      expect(auditActionLabel(a)).not.toBe(a)
    }
    for (const a of auditActions) expect(AUDIT_ACTION_LABEL[a]).toBeTruthy()
  })

  it('every task kind of the contract has words, and the eight new ones are in the contract', () => {
    const kinds = schemas.TaskKind?.enum as string[]
    for (const k of NEW_TASK_KINDS) {
      expect(kinds).toContain(k)
      expect(TASK_KIND_LABEL[k]).toBeTruthy()
      expect(taskKindLabel(k)).not.toBe(k)
    }
  })

  it('the task subject type ledgerKey is in the contract and is told in words; others are shown as sent', () => {
    expect(schemas.TaskSubject).toBeDefined()
    const text = JSON.stringify(spec.components.schemas.TaskSubject)
    expect(text).toContain('ledgerKey')
    expect(taskSubjectText({ type: 'ledgerKey', id: 'signing' })).toBe('Signing keys of the ledger')
    expect(taskSubjectText({ type: 'database', id: 'shop' })).toBe('database shop')
    // An id the panel does not know stays as sent.
    expect(taskSubjectText({ type: 'ledgerKey', id: 'other' })).toBe('ledgerKey other')
  })

  it('the five new problem codes are in the contract and have a heading and an explanation', () => {
    const codes = schemas.ProblemCode?.enum as string[]
    for (const code of NEW_CODES) {
      expect(codes).toContain(code)
      const view = describeProblem(
        new ApiError({ type: 'about:blank', title: code, status: 409, code: code as ProblemCode }),
      )
      expect(view.heading).not.toBe(code)
      expect(view.message.length).toBeGreaterThan(20)
      expect(view.message).not.toContain(code)
    }
  })

  it('the 28 operations are in the contract: 160 in all (0.17.0)', () => {
    expect(Object.keys(operations)).toHaveLength(160)
    for (const a of NEW_ACTIONS) expect(a in operations, a).toBe(true)
  })
})

async function settle(taskId: number): Promise<void> {
  await waitFor(async () => {
    const t = await endpoints.getTask(taskId)
    expect(['done', 'failed']).toContain(t.state)
  })
}

describe('the mock: settings and signing', () => {
  beforeEach(async () => {
    await signInAs(createPinia())
  })

  it('answers the settings, changes them in range and refuses the rest', async () => {
    const settings = await endpoints.getLedgerSettings()
    ok(settings, 'LedgerSettingsCluster')
    expect(settings.publication.originPrefixLocked).toBe(true)
    const changed = await endpoints.updateLedgerSettings({
      publication: { intervalSeconds: 120 },
      anchor: { required: true },
    })
    ok(changed, 'LedgerSettingsCluster')
    expect(changed.publication.intervalSeconds).toBe(120)
    expect(changed.anchor.required).toBe(true)
    await expect(
      endpoints.updateLedgerSettings({ publication: { intervalSeconds: 5 } }),
    ).rejects.toMatchObject({ code: 'validation_failed' })
    await expect(
      endpoints.updateLedgerSettings({ anchor: { minimumAnchored: 9 } }),
    ).rejects.toMatchObject({ code: 'validation_failed' })
    // A key exists, so the prefix is locked; the same value is not a change.
    await expect(
      endpoints.updateLedgerSettings({ publication: { originPrefix: 'other.example.com/x' } }),
    ).rejects.toMatchObject({ code: 'origin_prefix_locked' })
    ok(
      await endpoints.updateLedgerSettings({
        publication: { originPrefix: settings.publication.originPrefix },
      }),
      'LedgerSettingsCluster',
    )
    expect(getState().audit[0]?.action).toBe('updateLedgerSettings')
  })

  it('lets the origin prefix change before the first key, and locks it with the key', async () => {
    applySwitches(new URLSearchParams('signing=none'), getState())
    const before = await endpoints.getLedgerSettings()
    expect(before.publication.originPrefixLocked).toBe(false)
    const changed = await endpoints.updateLedgerSettings({
      publication: { originPrefix: 'ledger.example.com/kingres' },
    })
    expect(changed.publication.originPrefix).toBe('ledger.example.com/kingres')
    const task = await endpoints.createLedgerSigningKey({ backupMade: true })
    expect(task.kind).toBe('ledger.key.create')
    expect(task.subject).toEqual({ type: 'ledgerKey', id: 'signing' })
    await settle(task.id)
    const signing = await endpoints.getLedgerSigning()
    ok(signing, 'LedgerSigning')
    expect(signing.keys).toHaveLength(1)
    expect(signing.clusterId).toMatch(/^[0-9a-f]{64}$/)
    expect(signing.backupAdvised).toBe(false)
    expect((await endpoints.getLedgerSettings()).publication.originPrefixLocked).toBe(true)
    await expect(
      endpoints.updateLedgerSettings({ publication: { originPrefix: 'another.example.com/x' } }),
    ).rejects.toMatchObject({ code: 'origin_prefix_locked' })
  })

  it('answers the signing state without a secret in it', async () => {
    const signing = await endpoints.getLedgerSigning()
    ok(signing, 'LedgerSigning')
    for (const key of signing.keys) ok(key, 'LedgerSigningKey')
    for (const event of signing.events) ok(event, 'LedgerSigningEvent')
    expect(JSON.stringify(signing)).not.toMatch(/seed|passphrase"|privateKey|private_key/i)
    expect(signing.holders.length).toBeGreaterThan(0)
  })

  it('rotates, then retires and refuses to retire the last key, one task at a time', async () => {
    const signing = await endpoints.getLedgerSigning()
    const active = signing.keys.find((k) => k.state === 'active')!
    // The only key in force cannot be retired or revoked: a revocation never creates a key.
    await expect(endpoints.retireLedgerSigningKey(active.fingerprint)).rejects.toMatchObject({
      code: 'last_signing_key',
    })
    await expect(
      endpoints.revokeLedgerSigningKey(active.fingerprint, {
        compromisedSince: new Date().toISOString(),
        reason: 'lost',
        confirm: active.fingerprint,
      }),
    ).rejects.toMatchObject({ code: 'last_signing_key' })
    const rotation = await endpoints.createLedgerSigningKey({ overlapDays: 7 })
    expect(rotation.kind).toBe('ledger.key.rotate')
    await expect(endpoints.createLedgerSigningKey()).rejects.toMatchObject({
      code: 'operation_in_progress',
    })
    await settle(rotation.id)
    const during = await endpoints.getLedgerSigning()
    expect(during.keys.filter((k) => k.state === 'active')).toHaveLength(2)
    expect(during.backupAdvised).toBe(true)
    const retire = await endpoints.retireLedgerSigningKey(active.fingerprint)
    expect(retire.kind).toBe('ledger.key.retire')
    await settle(retire.id)
    const after = await endpoints.getLedgerSigning()
    ok(after, 'LedgerSigning')
    expect(after.keys.find((k) => k.fingerprint === active.fingerprint)?.state).toBe('retired')
    await expect(endpoints.retireLedgerSigningKey('0'.repeat(64))).rejects.toMatchObject({
      code: 'not_found',
    })
  })

  it('revokes with the fingerprint typed, clamps the time and keeps the reason printable', async () => {
    applySwitches(new URLSearchParams('signing=rotating'), getState())
    const signing = await endpoints.getLedgerSigning()
    const target = signing.keys.find((k) => k.state === 'active')!
    const request = {
      compromisedSince: new Date(Date.now() + 86_400_000).toISOString(),
      reason: 'laptop lost',
      confirm: target.fingerprint,
    }
    await expect(
      endpoints.revokeLedgerSigningKey(target.fingerprint, { ...request, confirm: 'nope' }),
    ).rejects.toMatchObject({ code: 'validation_failed' })
    await expect(
      endpoints.revokeLedgerSigningKey(target.fingerprint, {
        ...request,
        reason: `caf${String.fromCharCode(0xe9)}`,
      }),
    ).rejects.toMatchObject({ code: 'validation_failed' })
    const task = await endpoints.revokeLedgerSigningKey(target.fingerprint, request)
    expect(task.kind).toBe('ledger.key.revoke')
    await settle(task.id)
    const key = (await endpoints.getLedgerSigning()).keys.find(
      (k) => k.fingerprint === target.fingerprint,
    )
    expect(key?.state).toBe('revoked')
    // Never later than the revocation.
    expect(Date.parse(key?.compromisedSince ?? '')).toBeLessThanOrEqual(Date.now())
  })

  it('ends a key task with the code the switch names, and changes nothing', async () => {
    applySwitches(new URLSearchParams('signing=fail:passphrase_mismatch'), getState())
    const before = (await endpoints.getLedgerSigning()).keys.length
    const task = await endpoints.createLedgerSigningKey()
    await settle(task.id)
    const done = await endpoints.getTask(task.id)
    expect(done.state).toBe('failed')
    expect(done.error?.code).toBe('passphrase_mismatch')
    expect((await endpoints.getLedgerSigning()).keys).toHaveLength(before)
    // The next one works: the failure was for one task.
    const again = await endpoints.createLedgerSigningKey()
    await settle(again.id)
    expect((await endpoints.getTask(again.id)).state).toBe('done')
  })

  it('refuses a public key in the mode file, and a rotation when no key is active', async () => {
    await expect(endpoints.createLedgerSigningKey({ publicKey: 'AAAA' })).rejects.toMatchObject({
      code: 'unsupported_operation',
    })
    const s = getState()
    for (const k of s.ledger7.signing.keys) k.state = 'retired'
    await expect(endpoints.createLedgerSigningKey()).rejects.toMatchObject({
      code: 'signing_key_missing',
    })
  })

  it('serves the trust file as the bytes of a download, and refuses it when no key exists', async () => {
    const text = await endpoints.getLedgerTrustFile()
    ok(JSON.parse(text), 'LedgerTrustFile')
    expect(JSON.parse(text).keys.length).toBeGreaterThan(0)
    expect(text).not.toMatch(/seed|passphrase/i)
    applySwitches(new URLSearchParams('signing=none'), getState())
    await expect(endpoints.getLedgerTrustFile()).rejects.toMatchObject({
      code: 'signing_key_missing',
    })
  })
})

describe('the mock: publication, aggregates and anchoring', () => {
  beforeEach(async () => {
    await signInAs(createPinia())
  })

  it('answers the publication with its logs, and none when it is off', async () => {
    const p = await endpoints.getLedgerPublication()
    ok(p, 'LedgerPublication')
    expect(p.logs.some((l) => l.kind === 'aggregate')).toBe(true)
    expect(p.logs.some((l) => l.kind === 'chain')).toBe(true)
    await endpoints.updateLedgerSettings({ publication: { enabled: false } })
    const off = await endpoints.getLedgerPublication()
    ok(off, 'LedgerPublication')
    expect(off.logs).toEqual([])
  })

  it('answers the status of a database with publication and anchoring, and blocks that are signed', async () => {
    const status = await endpoints.getLedgerStatus('shop')
    ok(status, 'LedgerStatus')
    expect(status.publication?.state).toBe('ok')
    expect(status.anchoring).toEqual({ targets: 0, newestAnchoredAt: null, state: 'none' })
    const blocks = await endpoints.listLedgerBlocks('shop', { limit: 10 })
    ok(blocks, 'LedgerBlockPage')
    expect(blocks.items.some((b) => b.signed)).toBe(true)
    expect(blocks.items.some((b) => !b.signed)).toBe(true)
    const digests = await endpoints.listLedgerDigests('shop')
    ok(digests, 'LedgerDigestPage')
    expect(digests.items[0]?.aggregateSize).toBe(41)
  })

  it('answers an aggregate with members, the statement and its anchors; and pages the list', async () => {
    const latest = await endpoints.getLedgerAggregate('shop')
    ok(latest, 'LedgerAggregate')
    expect(latest.members.length).toBeGreaterThan(0)
    expect(latest.statement).toMatch(/does NOT prove/)
    expect(latest.checkpoint).toContain(String(latest.size))
    const older = await endpoints.getLedgerAggregate('shop', 10)
    expect(older.size).toBe(10)
    await expect(endpoints.getLedgerAggregate('shop', 9999)).rejects.toMatchObject({
      code: 'not_found',
    })
    const page = await endpoints.listLedgerAggregates('shop', { limit: 10 })
    ok(page, 'LedgerAggregatePage')
    expect(page.items).toHaveLength(10)
    expect(page.nextCursor).not.toBeNull()
  })

  it('says ledger_not_published for a database without a published aggregate', async () => {
    await endpoints.updateLedgerSettings({ publication: { enabled: false } })
    await expect(endpoints.getLedgerAggregate('shop')).rejects.toMatchObject({
      code: 'ledger_not_published',
    })
    await expect(endpoints.anchorLedgerNow('shop')).rejects.toMatchObject({
      code: 'ledger_not_published',
    })
    await expect(
      endpoints.createLedgerBundle('shop', { includeAnchors: false }),
    ).rejects.toMatchObject({ code: 'ledger_not_published' })
  })

  it('marks a diverged chain and a stale member in the aggregate', async () => {
    applySwitches(new URLSearchParams('publication=diverged,unread'), getState())
    const agg = await endpoints.getLedgerAggregate('shop')
    ok(agg, 'LedgerAggregate')
    expect(agg.members.find((m) => m.shardGroupId === 2)?.flags).toContain('diverged')
    expect(agg.members.some((m) => m.flags.includes('stale'))).toBe(true)
    const logs = (await endpoints.getLedgerPublication()).logs
    expect(logs.some((l) => l.state === 'diverged')).toBe(true)
    expect((await endpoints.getLedgerStatus('shop')).publication?.state).toBe('diverged')
  })

  it('creates, changes, tests and deletes an anchor target; the credentials are never returned', async () => {
    const created = await endpoints.createLedgerAnchorTarget({
      kind: 'worm',
      name: 'vault',
      enabled: true,
      config: { region: 'eu-west-1', bucket: 'bbb' },
      credentials: { accessKeyId: 'AKIA...', secretAccessKey: 'hunter2' },
    })
    ok(created, 'LedgerAnchorTarget')
    expect(created.credentialsSet).toBe(true)
    expect(JSON.stringify(created)).not.toMatch(/hunter2|AKIA/)
    await expect(
      endpoints.createLedgerAnchorTarget({
        kind: 'worm',
        name: 'vault',
        config: { region: 'eu-west-1', bucket: 'bbb' },
      }),
    ).rejects.toMatchObject({ code: 'already_exists' })
    await expect(
      endpoints.updateLedgerAnchorTarget(created.id, { kind: 'tsa' }),
    ).rejects.toMatchObject({ code: 'immutable_field' })
    ok(
      await endpoints.updateLedgerAnchorTarget(created.id, { enabled: false }),
      'LedgerAnchorTarget',
    )
    ok(await endpoints.getLedgerAnchorTarget(created.id), 'LedgerAnchorTarget')
    ok(await endpoints.listLedgerAnchorTargets(), 'LedgerAnchorTargetList')
    const test = await endpoints.testLedgerAnchorTarget(created.id)
    ok(test, 'LedgerAnchorTest')
    ok(await endpoints.getLedgerAnchorTarget(created.id), 'LedgerAnchorTarget')
    await endpoints.deleteLedgerAnchorTarget(created.id)
    await expect(endpoints.getLedgerAnchorTarget(created.id)).rejects.toMatchObject({
      code: 'not_found',
    })
  })

  it('anchors now, retries a failed record and downloads evidence', async () => {
    await expect(endpoints.anchorLedgerNow('shop')).rejects.toMatchObject({ code: 'conflict' })
    applySwitches(new URLSearchParams('anchors=failed'), getState())
    const now = await endpoints.anchorLedgerNow('shop')
    ok(now, 'LedgerAnchorList')
    expect(now.items[0]?.state).toBe('pending')
    const page = await endpoints.listLedgerAnchors('shop', { state: 'failed' })
    ok(page, 'LedgerAnchorPage')
    const failed = page.items[0]!
    ok(await endpoints.retryLedgerAnchor('shop', failed.id), 'LedgerAnchor')
    await expect(endpoints.retryLedgerAnchor('shop', failed.id)).rejects.toMatchObject({
      code: 'conflict',
    })
    const anchored = (await endpoints.listLedgerAnchors('shop', { state: 'anchored' })).items[0]!
    const evidence = await endpoints.downloadLedgerAnchor('shop', anchored.id, 'json')
    expect(evidence.size).toBeGreaterThan(0)
    await expect(endpoints.downloadLedgerAnchor('shop', anchored.id, 'ots')).rejects.toMatchObject({
      code: 'unprocessable',
    })
    expect((await endpoints.getLedgerStatus('shop')).anchoring?.targets).toBe(1)
  })

  it('creates a bundle, registers and revokes client keys, sets a policy, upgrades, acknowledges', async () => {
    const bundle = await endpoints.createLedgerBundle('shop', { includeAnchors: false })
    expect(bundle.kind).toBe('ledger.bundle.create')
    await settle(bundle.id)
    expect((await endpoints.getLedgerBundle('shop', 1)).size).toBeGreaterThan(0)

    const key = 'A'.repeat(43) + '='
    const registered = await endpoints.registerLedgerClientKey('shop', {
      publicKey: key,
      label: 'ingest',
    })
    ok(registered, 'LedgerClientKey')
    await expect(
      endpoints.registerLedgerClientKey('shop', { publicKey: key, label: 'again' }),
    ).rejects.toMatchObject({ code: 'already_exists' })
    await expect(
      endpoints.registerLedgerClientKey('shop', { publicKey: 'short', label: 'x' }),
    ).rejects.toMatchObject({ code: 'validation_failed' })
    ok(await endpoints.listLedgerClientKeys('shop'), 'LedgerClientKeyList')
    ok(
      await endpoints.revokeLedgerClientKey('shop', registered.keyId, 'rotated'),
      'LedgerClientKey',
    )
    await expect(
      endpoints.revokeLedgerClientKey('shop', registered.keyId, 'again'),
    ).rejects.toMatchObject({ code: 'conflict' })

    const table = getState()
      .databases.find((d) => d.info.name === 'shop')!
      .tables.find((t) => t.ledger)!
    ok(await endpoints.setTableSignaturePolicy('shop', table.schema, table.name), 'Table')
    await expect(
      endpoints.setTableSignaturePolicy('shop', table.schema, table.name),
    ).rejects.toMatchObject({ code: 'conflict' })

    const upgrade = await endpoints.upgradeLedgerExtension('shop')
    expect(upgrade.kind).toBe('ledger.upgrade')
    expect(upgrade.subject).toEqual({ type: 'database', id: 'shop' })

    await expect(
      endpoints.acknowledgeLedgerDivergence('shop', 2, {
        kind: 'restore',
        reason: 'x',
        confirm: 'shop',
      }),
    ).rejects.toMatchObject({ code: 'chain_not_diverged' })
    applySwitches(new URLSearchParams('publication=diverged'), getState())
    await expect(
      endpoints.acknowledgeLedgerDivergence('shop', 2, {
        kind: 'restore',
        reason: 'x',
        confirm: 'no',
      }),
    ).rejects.toMatchObject({ code: 'validation_failed' })
    const epoch = await endpoints.acknowledgeLedgerDivergence('shop', 2, {
      kind: 'restore',
      reason: 'restored a backup',
      confirm: 'shop',
    })
    expect(epoch.kind).toBe('ledger.epoch.start')
    await settle(epoch.id)
    const logs = (await endpoints.getLedgerPublication()).logs
    expect(logs.some((l) => l.state === 'closed')).toBe(true)
    expect(getState().audit.map((a) => a.action)).toContain('acknowledgeLedgerDivergence')
  })

  it('raises the alerts of the new kinds from the state, with the subjects the contract names', async () => {
    applySwitches(
      new URLSearchParams('signing=alarm&publication=diverged,behind&anchors=required'),
      getState(),
    )
    await endpoints.getLedgerSettings()
    const alerts = (await endpoints.listAlerts({ limit: 200 })).items
    for (const a of alerts) ok(a, 'Alert')
    const kinds = alerts.map((a) => a.kind)
    for (const k of [
      'ledgerSignerUnavailable',
      'ledgerChainDiverged',
      'ledgerPublicationBehind',
      'ledgerNotAnchored',
    ])
      expect(kinds, k).toContain(k)
    const diverged = alerts.find((a) => a.kind === 'ledgerChainDiverged')
    expect(diverged?.subject).toMatchObject({ databaseName: 'shop', shardGroupId: 2 })
    // Without an enabled target the absence of anchors is a warning when the setting says so, and ends with a target.
    expect(alerts.find((a) => a.kind === 'ledgerNotAnchored')?.severity).toBe('warning')
    applySwitches(new URLSearchParams('anchors=failed'), getState())
    const later = (await endpoints.listAlerts({ limit: 200 })).items
    expect(later.find((a) => a.kind === 'ledgerNotAnchored')?.state).toBe('resolved')
    const failed = later.find((a) => a.kind === 'ledgerAnchorFailed')
    expect(failed?.severity).toBe('critical')
    expect(failed?.subject?.aspect).toMatch(/^target:\d+$/)
  })
})

describe('the mock with hostile text (?mock=hostile)', () => {
  it('still answers the new operations with the shapes of the contract; the text is only text', async () => {
    seedHostile(getState())
    await signInAs(createPinia())
    ok(await endpoints.getLedgerSettings(), 'LedgerSettingsCluster')
    const signing = await endpoints.getLedgerSigning()
    ok(signing, 'LedgerSigning')
    expect(signing.signerAlarm).toContain('first line')
    ok(await endpoints.getLedgerPublication(), 'LedgerPublication')
    const agg = await endpoints.getLedgerAggregate('shop')
    ok(agg, 'LedgerAggregate')
    expect(agg.members.some((m) => m.flags.some((f) => f.includes('<img')))).toBe(true)
  })
})

describe('the mock as the real server is today (?phase7=today)', () => {
  beforeEach(async () => {
    await signInAs(createPinia())
    applySwitches(new URLSearchParams('phase7=today'), getState())
  })

  it('answers 422 unsupported_operation for the 21 operations that are not built, and works for the seven', async () => {
    ok(await endpoints.getLedgerSettings(), 'LedgerSettingsCluster')
    ok(await endpoints.getLedgerSigning(), 'LedgerSigning')
    expect((await endpoints.getLedgerTrustFile()).length).toBeGreaterThan(0)
    const calls: Record<string, () => Promise<unknown>> = {
      getLedgerPublication: () => endpoints.getLedgerPublication(),
      listLedgerAnchorTargets: () => endpoints.listLedgerAnchorTargets(),
      createLedgerAnchorTarget: () =>
        endpoints.createLedgerAnchorTarget({ kind: 'worm', name: 'x', config: {} }),
      getLedgerAnchorTarget: () => endpoints.getLedgerAnchorTarget(1),
      updateLedgerAnchorTarget: () => endpoints.updateLedgerAnchorTarget(1, { enabled: true }),
      deleteLedgerAnchorTarget: () => endpoints.deleteLedgerAnchorTarget(1),
      testLedgerAnchorTarget: () => endpoints.testLedgerAnchorTarget(1),
      getLedgerAggregate: () => endpoints.getLedgerAggregate('shop'),
      listLedgerAggregates: () => endpoints.listLedgerAggregates('shop'),
      listLedgerAnchors: () => endpoints.listLedgerAnchors('shop'),
      anchorLedgerNow: () => endpoints.anchorLedgerNow('shop'),
      retryLedgerAnchor: () => endpoints.retryLedgerAnchor('shop', 1),
      downloadLedgerAnchor: () => endpoints.downloadLedgerAnchor('shop', 1, 'json'),
      createLedgerBundle: () => endpoints.createLedgerBundle('shop', { includeAnchors: false }),
      getLedgerBundle: () => endpoints.getLedgerBundle('shop', 1),
      listLedgerClientKeys: () => endpoints.listLedgerClientKeys('shop'),
      registerLedgerClientKey: () =>
        endpoints.registerLedgerClientKey('shop', { publicKey: 'A'.repeat(43) + '=', label: 'x' }),
      revokeLedgerClientKey: () => endpoints.revokeLedgerClientKey('shop', 'abcd1234', 'x'),
      setTableSignaturePolicy: () => endpoints.setTableSignaturePolicy('shop', 'public', 'orders'),
      upgradeLedgerExtension: () => endpoints.upgradeLedgerExtension('shop'),
      acknowledgeLedgerDivergence: () =>
        endpoints.acknowledgeLedgerDivergence('shop', 1, {
          kind: 'other',
          reason: 'x',
          confirm: 'shop',
        }),
    }
    expect(Object.keys(calls)).toHaveLength(21)
    // The mock's list of what is not built is the list of these calls; with the seven, they are the 28 of 0.11.0.
    expect([...NOT_BUILT_YET].sort()).toEqual(Object.keys(calls).sort())
    expect(BUILT.length + NOT_BUILT_YET.length).toBe(28)
    for (const op of [...BUILT, ...NOT_BUILT_YET]) expect(op in operations, op).toBe(true)
    for (const [op, call] of Object.entries(calls))
      await expect(call(), op).rejects.toMatchObject({ code: 'unsupported_operation' })
  })
})

describe('the Alerts page with the new kinds', () => {
  it('names the alert in words and the anchor target of its subject; a hostile qualifier stays text', async () => {
    applySwitches(new URLSearchParams('anchors=failed'), getState())
    getState().alerts.unshift({
      id: 9001,
      kind: 'ledgerAnchorBehind',
      severity: 'warning',
      state: 'open',
      title: 'Behind',
      message: 'm',
      serverId: null,
      databaseName: null,
      raisedAt: new Date().toISOString(),
      subject: {
        serverId: null,
        behindServerId: null,
        shardGroupId: null,
        databaseName: null,
        aspect: 'target:<svg onload=x>',
      },
    })
    await renderApp('/alerts')
    expect(await screen.findByText('Ledger anchoring failed')).toBeInTheDocument()
    expect(screen.getByText('Ledger anchoring is behind')).toBeInTheDocument()
    expect(await screen.findByText('Anchor target 1')).toBeInTheDocument()
    expect(document.querySelector('svg[onload]')).toBeNull()
    expect(
      screen.getAllByTestId('alert-aspect').some((e) => /Concerns/.test(e.textContent ?? '')),
    ).toBe(true)
  })
})
