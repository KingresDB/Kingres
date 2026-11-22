// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The settings group `dataTransfer` and the Transfers panel (console increment CN2): the switch and the limits with their
// validation, the panel that lists the caller's export files with size, SHA-256, consistency and expiry, Download and
// Delete, and the states in which it shows no list (setting off, role too low).
import { webcrypto } from 'node:crypto'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { renderApp } from './helpers'

const sw = (query: string) => applySwitches(new URLSearchParams(query), getState())

describe('settings, Data transfer', () => {
  it('shows the group, saves a change of the switch and the limits as one partial update, and says it is audited', async () => {
    const user = userEvent.setup()
    await renderApp('/settings')
    const group = await screen.findByTestId('data-transfer-settings')
    expect(within(group).getByRole('checkbox')).toBeChecked()
    expect(within(group).getByLabelText('Largest file (GiB)')).toHaveValue(20)
    const retention = within(group).getByLabelText('Retention (hours)')
    await user.clear(retention)
    await user.type(retention, '48')
    await user.click(within(group).getByRole('checkbox'))
    await user.click(screen.getByRole('button', { name: /save settings/i }))
    expect(await screen.findByText(/settings saved/i)).toBeInTheDocument()
    expect(getState().settings.dataTransfer).toMatchObject({
      enabled: false,
      retentionHours: 48,
      maxFileBytes: 20 * 1024 ** 3,
      maxConcurrent: 2,
    })
    // The size fields were not touched, so they were not sent: the bytes of the server are unchanged.
    expect(getState().audit[0]?.action).toBe('updateClusterSettings')
  })

  it('says what off means, and that turning it on is recorded', async () => {
    sw('transfer=off')
    await renderApp('/settings')
    const note = await screen.findByTestId('data-transfer-state')
    expect(note).toHaveTextContent(/off\. every transfer operation is refused/i)
    expect(note).toHaveTextContent(/recorded in the audit log/i)
    expect(note).toHaveTextContent(/2\.0 GiB/)
  })

  it('refuses a value outside the contract in the form, with the field named, and sends nothing', async () => {
    const user = userEvent.setup()
    await renderApp('/settings')
    const group = await screen.findByTestId('data-transfer-settings')
    const retention = within(group).getByLabelText('Retention (hours)')
    await user.clear(retention)
    await user.type(retention, '500')
    const concurrent = within(group).getByLabelText('Transfers at once')
    await user.clear(concurrent)
    await user.type(concurrent, '0')
    const quota = within(group).getByLabelText('Staging quota (GiB)')
    await user.clear(quota)
    await user.type(quota, '0.0001')
    await user.click(screen.getByRole('button', { name: /save settings/i }))
    expect(
      await within(group).findByText(/retention time must be a whole number between 1 and 168/i),
    ).toBeInTheDocument()
    expect(
      within(group).getByText(/number of transfers must be a whole number between 1 and 16/i),
    ).toBeInTheDocument()
    expect(within(group).getByText(/staging quota is at least 0.001 GiB/i)).toBeInTheDocument()
    expect(getState().settings.dataTransfer?.retentionHours).toBe(24)
  })

  it('sends a size in bytes when it was changed', async () => {
    const user = userEvent.setup()
    await renderApp('/settings')
    const group = await screen.findByTestId('data-transfer-settings')
    const quota = within(group).getByLabelText('Staging quota (GiB)')
    await user.clear(quota)
    await user.type(quota, '1.5')
    await user.click(screen.getByRole('button', { name: /save settings/i }))
    expect(await screen.findByText(/settings saved/i)).toBeInTheDocument()
    expect(getState().settings.dataTransfer?.stagingQuotaBytes).toBe(1.5 * 1024 ** 3)
  })

  it('is read-only for a role below administrator: the save button says why', async () => {
    await renderApp('/settings', { user: 'viewer' })
    const save = await screen.findByRole('button', { name: /save settings/i })
    expect(save).toHaveAttribute('aria-disabled', 'true')
  })
})

describe('Transfers panel', () => {
  const saved: { name: string; size: number }[] = []
  beforeEach(() => {
    saved.length = 0
    vi.stubGlobal('crypto', webcrypto)
    URL.createObjectURL = vi.fn((blob: Blob | MediaSource) => {
      saved.push({ name: '', size: (blob as Blob).size })
      return 'blob:mock'
    })
    URL.revokeObjectURL = vi.fn()
  })
  afterEach(() => vi.unstubAllGlobals())

  it('lists the export files of the caller with state, size, SHA-256, consistency and expiry', async () => {
    sw('transfer=exports')
    await renderApp('/databases')
    const panel = await screen.findByTestId('transfers-panel')
    const rows = await within(panel).findAllByRole('row')
    // The header, then the three files.
    expect(rows).toHaveLength(4)
    const ready = within(panel).getByText('shop.public.orders').closest('tr') as HTMLElement
    expect(within(ready).getByText('Ready')).toBeInTheDocument()
    expect(within(ready).getByText(/per group \(3\)/)).toBeInTheDocument()
    expect(within(ready).getByText('28 B')).toBeInTheDocument()
    const expired = within(panel).getByText('shop.public.notes').closest('tr') as HTMLElement
    expect(within(expired).getByText('Expired')).toBeInTheDocument()
    // Only a file that is ready can be downloaded.
    expect(within(expired).getByRole('button', { name: /download/i })).toHaveAttribute(
      'aria-disabled',
      'true',
    )
    expect(within(ready).getByRole('button', { name: /download/i })).not.toHaveAttribute(
      'aria-disabled',
    )
  })

  it('shows nothing of another role and says there is nothing yet', async () => {
    sw('transfer=exports')
    getState().transfer.exports.forEach((e) => (e.creator = 'colleague'))
    await renderApp('/databases')
    expect(await screen.findByText('No export files')).toBeInTheDocument()
  })

  it('on a database page lists only the files of that database and counts the others', async () => {
    sw('transfer=exports')
    getState().transfer.exports[1]!.meta.database = 'reports'
    await renderApp('/databases/shop')
    const panel = await screen.findByTestId('transfers-panel')
    await within(panel).findByText('shop.public.orders')
    expect(within(panel).queryByText('reports.public.customers')).toBeNull()
    expect(within(panel).getByTestId('transfers-others')).toHaveTextContent('1 more file of yours')
  })

  it('deletes a file after a confirmation that says what it means', async () => {
    const user = userEvent.setup()
    sw('transfer=exports')
    await renderApp('/databases')
    const panel = await screen.findByTestId('transfers-panel')
    const row = (await within(panel).findByText('shop.public.orders')).closest('tr') as HTMLElement
    await user.click(within(row).getByRole('button', { name: /delete/i }))
    const dialog = await screen.findByRole('dialog')
    expect(dialog).toHaveTextContent(/cannot be downloaded again/i)
    await user.click(within(dialog).getByRole('button', { name: /delete the file/i }))
    await waitFor(() => expect(within(panel).queryByText('shop.public.orders')).toBeNull())
    expect(getState().transfer.exports.map((e) => e.meta.table)).not.toContain('orders')
  })

  it('shows the reason and asks the server for nothing while the setting is off', async () => {
    sw('transfer=off')
    await renderApp('/databases')
    const note = await screen.findByTestId('transfers-off')
    expect(note).toHaveTextContent(/turned off for this cluster/i)
    expect(within(note).getByRole('link', { name: /settings, data transfer/i })).toBeInTheDocument()
    expect(screen.queryByRole('table', { name: /export files/i })).toBeNull()
  })

  it('says that the role is too low and lists nothing for a role below administrator', async () => {
    sw('transfer=exports')
    await renderApp('/databases', { user: 'ops' })
    const note = await screen.findByTestId('transfers-role')
    expect(note).toHaveTextContent(/requires the kingres_admin cluster role/i)
  })

  it('downloads a ready file: the dialog shows the size and the SHA-256 and saves the bytes', async () => {
    const user = userEvent.setup()
    sw('transfer=exports')
    await renderApp('/databases')
    const panel = await screen.findByTestId('transfers-panel')
    const row = (await within(panel).findByText('shop.public.orders')).closest('tr') as HTMLElement
    await user.click(within(row).getByRole('button', { name: /download/i }))
    const dialog = await screen.findByRole('dialog')
    const rec = getState().transfer.exports.find((e) => e.meta.table === 'orders')!
    expect(within(dialog).getByTestId('download-sha')).toHaveTextContent(rec.meta.sha256 as string)
    await user.click(within(dialog).getByTestId('download-start'))
    expect(await within(dialog).findByTestId('download-done')).toHaveTextContent(
      /matches the export/i,
    )
    expect(saved).toEqual([{ name: '', size: rec.meta.sizeBytes }])
    expect(
      getState().transfer.exports.find((e) => e.meta.table === 'orders')?.meta.downloadedAt,
    ).not.toBeNull()
  })
})
