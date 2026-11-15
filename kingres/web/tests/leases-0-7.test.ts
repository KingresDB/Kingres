// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.7.0, `ShardGroup.lease`: per shard group whether the primary holds its lease, whether the lease is
// enforced (the group has a standby) or only watched, the holder and the time left, and the lost state, on the
// Replication page and on the page of the server that runs the primary.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import { getState } from '@/mocks/http'
import { loseLease, mockConfig, regainLease } from '@/mocks/state'
import { HOSTILE } from '@/mocks/hostile'
import { formatLease } from '@/lib/lease'
import { renderApp } from './helpers'

const group = async (id: number) => within(await screen.findByTestId(`group-${id}`))

describe('leases on the Replication page', () => {
  it('shows a held lease with its holder, the time left and that it is enforced', async () => {
    await renderApp('/replication')
    // Group 1 runs its primary on the server that answers (10.0.0.11).
    const g = await group(1)
    const lease = g.getByTestId('group-lease')
    expect(lease).toHaveAttribute('data-lease-state', 'held')
    expect(within(lease).getByText('Lease held')).toBeInTheDocument()
    expect(within(lease).getByTestId('lease-text')).toHaveTextContent(
      /primary on server 10\.0\.0\.11 holds the lease/,
    )
    expect(within(lease).getByTestId('lease-remaining').textContent).toMatch(/^\d+\.\d s$/)
    expect(within(lease).getByTestId('lease-enforced')).toHaveTextContent('Enforced')
    expect(within(lease).getByTestId('lease-enforcement')).toHaveTextContent(/has a standby/)
  })

  it('says "granted" for a group whose primary runs on another server: what the answering server knows', async () => {
    await renderApp('/replication')
    const lease = (await group(2)).getByTestId('group-lease')
    expect(lease).toHaveAttribute('data-lease-state', 'granted')
    expect(within(lease).getByText('Lease granted')).toBeInTheDocument()
    expect(within(lease).getByTestId('lease-text')).toHaveTextContent(
      /granted the lease to server\s+10\.0\.0\.12/,
    )
    expect(within(lease).getByTestId('lease-text')).toHaveTextContent(/not a check of the primary/)
  })

  it('a lost enforced lease says in plain words that the primary was stopped on purpose', async () => {
    expect(loseLease(getState(), 1)).toBe(true)
    await renderApp('/replication')
    const g = await group(1)
    const lease = await waitFor(() => {
      const el = g.getByTestId('group-lease')
      expect(el).toHaveAttribute('data-lease-state', 'lost')
      return el
    })
    expect(within(lease).getByText('Lease lost')).toBeInTheDocument()
    expect(within(lease).getByTestId('lease-text')).toHaveTextContent(/stopped on purpose/i)
    expect(within(lease).getByTestId('lease-text')).toHaveTextContent(
      /two primaries can never run/i,
    )
    // The primary instance is stopped and the group is unavailable.
    expect(g.getByText('Unavailable')).toBeInTheDocument()
    expect(g.getAllByText('Stopped').length).toBeGreaterThan(0)
  })

  it('a lost lease of a group without a standby is not enforced: the primary keeps running', async () => {
    const s = getState()
    s.groups.find((x) => x.id === 1)!.standbyServerIds = []
    loseLease(s, 1)
    await renderApp('/replication')
    const lease = (await group(1)).getByTestId('group-lease')
    await waitFor(() => expect(lease).toHaveAttribute('data-lease-state', 'lost'))
    expect(within(lease).getByTestId('lease-enforced')).toHaveTextContent('Watched only')
    expect(within(lease).getByTestId('lease-text')).toHaveTextContent(/keeps running/i)
    expect(within(lease).getByTestId('lease-text')).not.toHaveTextContent(/stopped on purpose/i)
  })

  it('says "no lease" for a group without a primary', async () => {
    getState().groups.find((x) => x.id === 3)!.primaryServerId = null
    await renderApp('/replication')
    const lease = (await group(3)).getByTestId('group-lease')
    expect(lease).toHaveAttribute('data-lease-state', 'none')
    expect(within(lease).getByText('No lease')).toBeInTheDocument()
  })

  it('says so when the server reports no leases (an older revision)', async () => {
    mockConfig.leaseAbsent = true
    await renderApp('/replication')
    const lease = (await group(1)).getByTestId('group-lease')
    expect(lease).toHaveAttribute('data-lease-state', 'absent')
    expect(within(lease).getByTestId('lease-absent')).toHaveTextContent(/does not report leases/)
  })

  it('shows the state again as held after the lease is obtained', async () => {
    const s = getState()
    loseLease(s, 1)
    await renderApp('/replication')
    const g = await group(1)
    await waitFor(() =>
      expect(g.getByTestId('group-lease')).toHaveAttribute('data-lease-state', 'lost'),
    )
    regainLease(s, 1)
    await waitFor(
      () => expect(g.getByTestId('group-lease')).toHaveAttribute('data-lease-state', 'held'),
      {
        timeout: 6000,
      },
    )
  })

  it('a hostile lease holder is shown as text with its marks', async () => {
    const s = getState()
    s.groups.find((x) => x.id === 2)!.primaryServerId = HOSTILE.reversed
    await renderApp('/replication')
    const lease = (await group(2)).getByTestId('group-lease')
    expect(lease.textContent).toMatch(/U\+202E/)
    expect(document.querySelector('img[src="x"]')).toBeNull()
  })
})

describe('leases on the server page', () => {
  it('lists the groups whose primary the server runs, with their leases', async () => {
    await renderApp('/servers/10.0.0.11')
    const section = within(await screen.findByTestId('server-leases'))
    // The server runs the primaries of the map group and of group 1.
    expect(await section.findByTestId('lease-group-0')).toBeInTheDocument()
    expect(section.getByTestId('lease-group-1')).toBeInTheDocument()
    expect(section.queryByTestId('lease-group-2')).toBeNull()
    expect(within(section.getByTestId('lease-group-1')).getByTestId('group-lease')).toHaveAttribute(
      'data-lease-state',
      'held',
    )
    expect(section.getByRole('link', { name: 'Shard group 1' })).toHaveAttribute(
      'href',
      '/replication#g-1',
    )
  })

  it('a server that runs a primary for another server answers "granted" from the answering server', async () => {
    await renderApp('/servers/10.0.0.12')
    const section = within(await screen.findByTestId('server-leases'))
    const g2 = await section.findByTestId('lease-group-2')
    expect(within(g2).getByTestId('group-lease')).toHaveAttribute('data-lease-state', 'granted')
  })

  it('shows the lost state of a primary the server runs', async () => {
    loseLease(getState(), 0)
    await renderApp('/servers/10.0.0.11')
    const g0 = await within(await screen.findByTestId('server-leases')).findByTestId(
      'lease-group-0',
    )
    await waitFor(() =>
      expect(within(g0).getByTestId('group-lease')).toHaveAttribute('data-lease-state', 'lost'),
    )
    expect(g0).toHaveTextContent(/stopped on purpose/i)
  })

  it('says so for a server that runs no primary', async () => {
    const s = getState()
    s.groups.find((x) => x.id === 2)!.primaryServerId = '10.0.0.13'
    s.groups.find((x) => x.id === 2)!.standbyServerIds = []
    await renderApp('/servers/10.0.0.12')
    const section = within(await screen.findByTestId('server-leases'))
    expect(await section.findByText('No primary runs here')).toBeInTheDocument()
  })
})

describe('lease helpers', () => {
  it('formats the time left', () => {
    expect(formatLease(7200)).toBe('7.2 s')
    expect(formatLease(350)).toBe('350 ms')
    expect(formatLease(0)).toBe('no time')
    expect(formatLease(undefined)).toBe('')
    expect(formatLease(null)).toBe('')
  })

  it('the alert and the audit events of a lost lease are written by the manager, once', async () => {
    const s = getState()
    loseLease(s, 1)
    await renderApp('/alerts')
    await screen.findByText('The primary of shard group 1 lost its lease')
    await renderApp('/alerts')
    const actions = s.audit.map((e) => e.action)
    expect(actions.filter((a) => a === 'leaseLost')).toHaveLength(1)
    expect(actions.filter((a) => a === 'leaseFenced')).toHaveLength(1)
    regainLease(s, 1)
    await renderApp('/alerts')
    await waitFor(() => expect(s.audit.some((e) => e.action === 'leaseAcquired')).toBe(true))
  })
})
