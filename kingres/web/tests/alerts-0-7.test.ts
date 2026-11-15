// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.7.0: `Alert.subject` (typed: the server, the server that is behind, the shard group, the database, a
// qualifier) links an alert to what it is about, in the alert list and on the dashboard; the new alert kinds
// (leaseLost, groupWithoutSynchronousStandby, groupUnderReplicated) have words and a severity presentation.
import { describe, expect, it } from 'vitest'
import { screen, within } from '@testing-library/vue'
import { getState } from '@/mocks/http'
import { seedHostile } from '@/mocks/hostile'
import type { Alert, AlertSubject } from '@/api/types'
import { alertKindHint, alertKindLabel, ALERT_KIND_LABEL, SEVERITY } from '@/lib/status'
import { renderApp } from './helpers'

const subject = (over: Partial<AlertSubject> = {}): AlertSubject => ({
  serverId: null,
  behindServerId: null,
  shardGroupId: null,
  databaseName: null,
  aspect: null,
  ...over,
})

const alert = (id: number, over: Partial<Alert> = {}): Alert => ({
  id,
  kind: 'diskSpaceLow',
  severity: 'warning',
  state: 'open',
  title: `Alert ${id}`,
  message: 'A message that names no server at all.',
  serverId: null,
  databaseName: null,
  raisedAt: new Date().toISOString(),
  ...over,
})

async function row(title: string): Promise<HTMLElement> {
  const cell = (await screen.findAllByText(title)).find((el) => el.closest('tr'))
  return cell?.closest('tr') as HTMLElement
}

describe('the subject of an alert', () => {
  it('links to the server, the server that is behind, the group and the database, not to words in the message', async () => {
    getState().alerts.unshift(
      alert(801, {
        kind: 'replicationPending',
        // The message names other servers on purpose: the panel must not parse it.
        message: 'The leader waits for 10.0.0.99.',
        serverId: '10.0.0.11',
        subject: subject({ serverId: '10.0.0.11', behindServerId: '10.0.0.13' }),
      }),
      alert(802, {
        kind: 'leaseLost',
        severity: 'critical',
        serverId: '10.0.0.11',
        subject: subject({ serverId: '10.0.0.11', shardGroupId: 1 }),
      }),
      alert(803, {
        kind: 'ledgerPendingTooOld',
        serverId: '10.0.0.12',
        databaseName: 'shop',
        subject: subject({ serverId: '10.0.0.12', databaseName: 'shop' }),
      }),
    )
    await renderApp('/alerts')
    const pending = within(await row('Alert 801'))
    expect(pending.getByRole('link', { name: '10.0.0.11' })).toHaveAttribute(
      'href',
      '/servers/10.0.0.11',
    )
    expect(pending.getByTestId('alert-behind-server')).toHaveTextContent(/server that is behind/i)
    expect(pending.getByRole('link', { name: '10.0.0.13' })).toHaveAttribute(
      'href',
      '/servers/10.0.0.13',
    )
    // The address in the message was not made a link.
    expect(pending.queryByRole('link', { name: '10.0.0.99' })).toBeNull()

    const lease = within(await row('Alert 802'))
    expect(lease.getByRole('link', { name: 'Shard group 1' })).toHaveAttribute(
      'href',
      '/replication#g-1',
    )
    const ledger = within(await row('Alert 803'))
    expect(ledger.getByRole('link', { name: 'shop' })).toHaveAttribute('href', '/databases/shop')
    expect(ledger.getByRole('link', { name: '10.0.0.12' })).toBeInTheDocument()
  })

  it('uses the members of the alert itself when the server sends no subject (an older revision)', async () => {
    getState().alerts.unshift(alert(810, { serverId: '10.0.0.13', databaseName: 'shop' }))
    await renderApp('/alerts')
    const r = within(await row('Alert 810'))
    expect(r.getByRole('link', { name: '10.0.0.13' })).toHaveAttribute('href', '/servers/10.0.0.13')
    expect(r.getByRole('link', { name: 'shop' })).toBeInTheDocument()
  })

  it('says nothing for an alert about the cluster', async () => {
    getState().alerts.unshift(alert(811, { kind: 'versionMismatch', subject: subject() }))
    await renderApp('/alerts')
    expect(within(await row('Alert 811')).queryByTestId('alert-subject')).toBeNull()
  })

  it('names the cluster certificate authority, and shows another qualifier as text', async () => {
    getState().alerts.unshift(
      alert(812, {
        kind: 'certificateExpiring',
        subject: subject({ aspect: 'ca' }),
      }),
      alert(813, { subject: subject({ aspect: 'somethingNew' }) }),
    )
    await renderApp('/alerts')
    expect(within(await row('Alert 812')).getByTestId('alert-aspect')).toHaveTextContent(
      'Cluster certificate authority',
    )
    expect(within(await row('Alert 813')).getByTestId('alert-aspect')).toHaveTextContent(
      'Concerns somethingNew',
    )
  })

  it('does not link a server id that no URL can carry', async () => {
    getState().alerts.unshift(alert(814, { subject: subject({ serverId: '..' }), serverId: '..' }))
    await renderApp('/alerts')
    const r = within(await row('Alert 814'))
    expect(r.getByTestId('alert-server')).toHaveTextContent('..')
    expect(r.queryByRole('link')).toBeNull()
  })

  it('shows the subject of the open alerts on the dashboard', async () => {
    getState().alerts.unshift(
      alert(820, {
        kind: 'memoryPressure',
        severity: 'warning',
        serverId: '10.0.0.12',
        subject: subject({ serverId: '10.0.0.12', shardGroupId: 2 }),
      }),
    )
    await renderApp('/dashboard')
    const li = (await screen.findByText('Alert 820')).closest('li') as HTMLElement
    expect(within(li).getByRole('link', { name: 'Shard group 2' })).toHaveAttribute(
      'href',
      '/replication#g-2',
    )
    expect(within(li).getByRole('link', { name: '10.0.0.12' })).toBeInTheDocument()
  })

  it('hostile server ids, database names and qualifiers are shown as text, with their marks', async () => {
    seedHostile(getState())
    await renderApp('/alerts')
    const behind = await screen.findByTestId('alert-behind-server')
    // Nothing from the subject became an element.
    expect(document.querySelector('img[src="x"]')).toBeNull()
    expect(document.querySelector('svg[onload]')).toBeNull()
    // The ids are made visible and do not reorder what is around them.
    expect(behind.textContent).toMatch(/U\+200B/)
    const servers = screen.getAllByTestId('alert-server').map((el) => el.textContent ?? '')
    expect(servers.some((t) => /U\+202E/.test(t))).toBe(true)
    const aspects = screen.getAllByTestId('alert-aspect')
    const concerns = aspects.find((el) => /Concerns/.test(el.textContent ?? ''))
    expect(concerns).toBeDefined()
    expect(concerns?.querySelector('svg')).toBeNull()
    expect(concerns?.textContent).toContain('<svg')
  })
})

describe('the alert kinds of 0.7.0', () => {
  it('have words, an explanation and a severity presentation', () => {
    for (const kind of ['leaseLost', 'groupWithoutSynchronousStandby', 'groupUnderReplicated']) {
      expect(ALERT_KIND_LABEL[kind as keyof typeof ALERT_KIND_LABEL]).toBeTruthy()
      expect(alertKindLabel(kind)).not.toBe(kind)
      expect(alertKindHint(kind)).toBeTruthy()
    }
    for (const kind of ['serverSuspect', 'serverDown'])
      expect(alertKindHint(kind)).toMatch(/heartbeat/i)
    // Severity is icon plus text, and critical differs from warning by more than colour.
    expect(SEVERITY.critical.icon).not.toBe(SEVERITY.warning.icon)
    expect(SEVERITY.critical.label).toBe('Critical')
  })

  it('show in the list with the severity the contract gives them', async () => {
    getState().alerts.unshift(
      alert(830, { kind: 'leaseLost', severity: 'critical', title: 'Lease lost on 1' }),
      alert(831, {
        kind: 'groupWithoutSynchronousStandby',
        severity: 'critical',
        title: 'No sync standby',
      }),
      alert(832, { kind: 'groupUnderReplicated', severity: 'warning', title: 'Too few standbys' }),
    )
    await renderApp('/alerts')
    const a = within(await row('Lease lost on 1'))
    expect(a.getByText('Critical')).toBeInTheDocument()
    expect(a.getByText('Primary lost its lease')).toBeInTheDocument()
    expect(a.getByTestId('alert-kind-hint')).toHaveTextContent(/stopped on purpose/i)
    const b = within(await row('No sync standby'))
    expect(b.getByText('Commits no longer wait for a standby')).toBeInTheDocument()
    expect(b.getByTestId('alert-kind-hint')).toHaveTextContent(/one server only/i)
    const c = within(await row('Too few standbys'))
    expect(c.getByText('Warning')).toBeInTheDocument()
    expect(c.getByText('Shard group has too few standbys')).toBeInTheDocument()
  })
})
