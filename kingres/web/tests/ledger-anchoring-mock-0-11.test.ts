// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The mock states of the Anchoring tab (Console B): every URL switch of `?anchors=` reaches its state, what the mock
// answers in it satisfies the schemas of the contract, a configuration of the wrong shape is refused with the field named,
// the credentials are never part of an answer, and `?phase7=today` makes every anchoring operation answer 422
// `unsupported_operation`, as the real server does today.
import { beforeEach, describe, expect, it } from 'vitest'
import { createPinia } from 'pinia'
import { endpoints } from '@/api/endpoints'
import { ApiError } from '@/api/problem'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { mockConfig } from '@/mocks/state'
import { ok } from './schema-check'
import { signInAs } from './helpers'

const sw = (query: string) => applySwitches(new URLSearchParams(query), getState())

beforeEach(async () => {
  await signInAs(createPinia())
})

describe('the switches of ?anchors=', () => {
  it('all: three kinds, a record in every state, and every answer satisfies the contract', async () => {
    sw('anchors=all')
    const list = await endpoints.listLedgerAnchorTargets()
    ok(list, 'LedgerAnchorTargetList')
    expect(list.items.map((t) => t.kind).sort()).toEqual(['ots', 'tsa', 'worm'])
    const page = await endpoints.listLedgerAnchors('shop', { limit: 100 })
    ok(page, 'LedgerAnchorPage')
    expect(new Set(page.items.map((r) => r.state))).toEqual(
      new Set(['anchored', 'pending', 'submitted', 'failed', 'expired']),
    )
    for (const t of list.items)
      ok(await endpoints.getLedgerAnchorTarget(t.id), 'LedgerAnchorTarget')
  })

  it('disabled: a target exists and nothing is anchored; the status says none and the alert is raised', async () => {
    sw('anchors=disabled')
    const [t] = (await endpoints.listLedgerAnchorTargets()).items
    expect(t).toMatchObject({ enabled: false, state: 'none', newestAnchoredAt: null })
    const status = await endpoints.getLedgerStatus('shop')
    expect(status.anchoring).toMatchObject({ targets: 0, state: 'none' })
  })

  it('testfail and testwarn change what a test finds, the shape stays the contract', async () => {
    sw('anchors=ok,testfail')
    const [t] = (await endpoints.listLedgerAnchorTargets()).items
    const failed = await endpoints.testLedgerAnchorTarget(t!.id)
    ok(failed, 'LedgerAnchorTest')
    expect(failed.ok).toBe(false)
    expect(failed.checks[0]).toMatchObject({ status: 'failed' })
    mockConfig.anchorTest = 'warn'
    const warned = await endpoints.testLedgerAnchorTarget(t!.id)
    ok(warned, 'LedgerAnchorTest')
    expect(warned.ok).toBe(true)
    expect(warned.checks.some((c) => c.status === 'warning')).toBe(true)
  })

  it('refuse:<code> answers that problem code to every change, and reads still work', async () => {
    sw('anchors=ok,refuse:forbidden')
    ok(await endpoints.listLedgerAnchorTargets(), 'LedgerAnchorTargetList')
    const [t] = (await endpoints.listLedgerAnchorTargets()).items
    const calls: Record<string, () => Promise<unknown>> = {
      create: () =>
        endpoints.createLedgerAnchorTarget({
          kind: 'ots',
          name: 'x',
          config: { calendars: ['https://a.example'] },
        }),
      update: () => endpoints.updateLedgerAnchorTarget(t!.id, { enabled: false }),
      delete: () => endpoints.deleteLedgerAnchorTarget(t!.id),
      test: () => endpoints.testLedgerAnchorTarget(t!.id),
      now: () => endpoints.anchorLedgerNow('shop'),
      retry: () => endpoints.retryLedgerAnchor('shop', 1),
    }
    for (const [name, call] of Object.entries(calls))
      await expect(call(), name).rejects.toMatchObject({ code: 'forbidden' })
    expect(getState().ledger7.targets).toHaveLength(1)
  })
})

describe('what the mock refuses, with the field named', () => {
  const worm = { region: 'eu-west-1', bucket: 'ledger-anchors' }
  const refused = async (
    kind: 'worm' | 'tsa' | 'ots',
    config: Record<string, unknown>,
    credentials?: Record<string, unknown>,
  ) => {
    try {
      await endpoints.createLedgerAnchorTarget({
        kind,
        name: 'n',
        config,
        ...(credentials ? { credentials } : {}),
      })
    } catch (e) {
      expect(e).toBeInstanceOf(ApiError)
      expect((e as ApiError).code).toBe('validation_failed')
      return ((e as ApiError).problem.errors ?? []) as { field: string }[]
    }
    return undefined
  }
  it('a missing bucket, a setting of another kind, a range, a calendar without TLS', async () => {
    expect((await refused('worm', { region: 'r' }))?.map((x) => x.field)).toContain('config.bucket')
    expect((await refused('worm', { ...worm, url: 'x' }))?.map((x) => x.field)).toContain(
      'config.url',
    )
    expect((await refused('worm', { ...worm, retentionDays: 0 }))?.map((x) => x.field)).toContain(
      'config.retentionDays',
    )
    expect((await refused('tsa', { url: 'https://t.example' }))?.map((x) => x.field)).toContain(
      'config.caBundlePath',
    )
    expect(
      (await refused('ots', { calendars: ['http://a.example'] }))?.map((x) => x.field),
    ).toContain('config.calendars')
    expect(getState().ledger7.targets).toHaveLength(0)
  })
  it('a credential of another kind, and half of a pair', async () => {
    expect((await refused('worm', worm, { password: 'x' }))?.map((x) => x.field)).toContain(
      'credentials.password',
    )
    expect((await refused('worm', worm, { accessKeyId: 'only' }))?.map((x) => x.field)).toContain(
      'credentials.secretAccessKey',
    )
  })
  it('never returns a credential, however it is asked', async () => {
    const t = await endpoints.createLedgerAnchorTarget({
      kind: 'worm',
      name: 'vault',
      enabled: true,
      config: worm,
      credentials: { accessKeyId: 'AKIASECRETID', secretAccessKey: 'hunter2-secret' },
    })
    ok(t, 'LedgerAnchorTarget')
    for (const text of [
      JSON.stringify(t),
      JSON.stringify(await endpoints.listLedgerAnchorTargets()),
      JSON.stringify(await endpoints.getLedgerAnchorTarget(t.id)),
      JSON.stringify(await endpoints.testLedgerAnchorTarget(t.id)),
      JSON.stringify(getState().ledger7.targets),
    ])
      expect(text).not.toMatch(/AKIASECRETID|hunter2/)
  })
})

describe('?phase7=today: the anchoring operations of a server that does not build them yet', () => {
  it('every one of the eleven answers 422 unsupported_operation', async () => {
    sw('phase7=today')
    const calls: Record<string, () => Promise<unknown>> = {
      listLedgerAnchorTargets: () => endpoints.listLedgerAnchorTargets(),
      createLedgerAnchorTarget: () =>
        endpoints.createLedgerAnchorTarget({ kind: 'ots', name: 'x', config: {} }),
      getLedgerAnchorTarget: () => endpoints.getLedgerAnchorTarget(1),
      updateLedgerAnchorTarget: () => endpoints.updateLedgerAnchorTarget(1, { enabled: true }),
      deleteLedgerAnchorTarget: () => endpoints.deleteLedgerAnchorTarget(1),
      testLedgerAnchorTarget: () => endpoints.testLedgerAnchorTarget(1),
      listLedgerAnchors: () => endpoints.listLedgerAnchors('shop'),
      anchorLedgerNow: () => endpoints.anchorLedgerNow('shop'),
      retryLedgerAnchor: () => endpoints.retryLedgerAnchor('shop', 1),
      downloadLedgerAnchor: () => endpoints.downloadLedgerAnchor('shop', 1, 'json'),
      getLedgerAggregate: () => endpoints.getLedgerAggregate('shop'),
    }
    for (const [name, call] of Object.entries(calls))
      await expect(call(), name).rejects.toMatchObject({
        code: 'unsupported_operation',
        status: 422,
      })
  })
})
