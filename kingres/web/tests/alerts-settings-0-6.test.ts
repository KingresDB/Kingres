// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.6.0: the alert kind `replicationPending` (label, explanation, link to the server) and the
// local settings whose WAL and log directory are always present, null when they are the default.
import { beforeEach, describe, expect, it } from 'vitest'
import { screen, within } from '@testing-library/vue'
import { getState } from '@/mocks/http'
import type { Alert } from '@/api/types'
import { alertKindHint, alertKindLabel } from '@/lib/status'
import { calmClocks, renderApp } from './helpers'

const pending = (over: Partial<Alert> = {}): Alert => ({
  id: 700,
  kind: 'replicationPending',
  severity: 'warning',
  state: 'open',
  title: 'A change has not reached server 10.0.0.13',
  message: 'The leader waits to hand a change of roles to server 10.0.0.13.',
  serverId: '10.0.0.13',
  databaseName: null,
  raisedAt: new Date().toISOString(),
  ...over,
})

// The seed has a server whose clock is 1.82 s off (0.8.0), which raises the alert clockDifference; these tests count alerts.
beforeEach(calmClocks)

describe('alert kind replicationPending (0.6.0)', () => {
  it('has a label and an explanation that says what can still work on the server', () => {
    expect(alertKindLabel('replicationPending')).toBe('A change has not reached a server yet')
    expect(alertKindHint('replicationPending')).toMatch(
      /disabled, changed or dropped may still work/,
    )
    expect(alertKindHint('diskSpaceLow')).toBeNull()
  })

  it('shows the label, the explanation and a link to the server', async () => {
    getState().alerts.unshift(pending())
    await renderApp('/alerts')
    const where = (await screen.findAllByTestId('alert-server'))[0] as HTMLElement
    expect(within(where).getByRole('link', { name: '10.0.0.13' })).toHaveAttribute(
      'href',
      '/servers/10.0.0.13',
    )
    expect(screen.getByText('A change has not reached a server yet')).toBeInTheDocument()
    // Other kinds with a sentence of their own (serverDown, serverSuspect) show theirs too.
    expect(
      screen
        .getAllByTestId('alert-kind-hint')
        .some((el) => /may still work on that server/.test(el.textContent ?? '')),
    ).toBe(true)
  })

  it('a server id that no URL can carry stays plain text', async () => {
    getState().alerts.unshift(pending({ id: 701, serverId: '..' }))
    await renderApp('/alerts')
    const where = (await screen.findAllByTestId('alert-server'))[0] as HTMLElement
    expect(within(where).queryByRole('link')).toBeNull()
  })
})

describe('local settings: directories that are the default (0.6.0)', () => {
  it('shows "default" for a null WAL and log directory, not an empty cell', async () => {
    const l = getState().localSettings
    l.dataPaths = {
      dataDirectory: l.dataPaths.dataDirectory,
      walDirectory: null,
      logDirectory: null,
    }
    await renderApp('/this-server')
    const wal = await screen.findByTestId('wal-directory')
    expect(wal).toHaveTextContent(/^default \(inside the data directory\)$/)
    expect(screen.getByTestId('log-directory')).toHaveTextContent(
      /^default \(the system default\)$/,
    )
    // The log directory field keeps its placeholder: the default is "nothing typed".
    expect(screen.getByLabelText('Log directory')).toHaveValue('')
  })

  it('shows the path when one is set', async () => {
    const l = getState().localSettings
    l.dataPaths = { ...l.dataPaths, walDirectory: '/mnt/wal', logDirectory: '/var/log/kingresdb' }
    await renderApp('/this-server')
    expect(await screen.findByTestId('wal-directory')).toHaveTextContent('/mnt/wal')
    expect(screen.getByTestId('log-directory')).toHaveTextContent('/var/log/kingresdb')
  })
})
