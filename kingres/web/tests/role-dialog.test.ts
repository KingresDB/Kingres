// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// W-07: a role saved without changes sends nothing, whatever the browser time zone is, and a date
// the person types is read as UTC (the zone the server and the rest of the panel use).
import { afterAll, beforeAll, describe, expect, it } from 'vitest'
import { fireEvent, screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { getState } from '@/mocks/http'
import { mockServer } from '@/mocks/server'
import { renderApp } from './helpers'

const zone = process.env.TZ
beforeAll(() => {
  process.env.TZ = 'Asia/Tehran'
})
afterAll(() => {
  if (zone === undefined) delete process.env.TZ
  else process.env.TZ = zone
})

function recordWrites(): { method: string; path: string; body: unknown }[] {
  const seen: { method: string; path: string; body: unknown }[] = []
  mockServer.events.on('request:start', async ({ request }) => {
    if (request.method === 'GET' || request.url.includes('/auth/')) return
    const text = await request.clone().text()
    seen.push({
      method: request.method,
      path: new URL(request.url).pathname,
      body: text ? JSON.parse(text) : null,
    })
  })
  return seen
}

async function editRole(name: string) {
  const user = userEvent.setup()
  const app = await renderApp('/security')
  const row = (await screen.findByRole('rowheader', { name })).closest('tr') as HTMLElement
  await user.click(within(row).getByRole('button', { name: /^edit$/i }))
  const dialog = await screen.findByRole('dialog', { name: new RegExp(`edit role ${name}`, 'i') })
  return { user, dialog, ...app }
}

describe('saving a role (W-07)', () => {
  it('under a time zone other than UTC, a save without edits sends nothing', async () => {
    expect(new Date(2027, 0, 13, 18, 58).getTimezoneOffset()).toBe(-210)
    const writes = recordWrites()
    const before = getState().roles.find((r) => r.name === 'analyst')?.validUntil
    expect(before).toBeTruthy()
    const { user, dialog } = await editRole('analyst')
    await user.click(within(dialog).getByRole('button', { name: /^save role$/i }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(writes).toHaveLength(0)
    expect(getState().roles.find((r) => r.name === 'analyst')?.validUntil).toBe(before)
  })

  it('shows the date in UTC and says so', async () => {
    const { dialog } = await editRole('analyst')
    const field = within(dialog).getByLabelText(/password valid until/i) as HTMLInputElement
    expect(field.labels?.[0]?.textContent).toMatch(/UTC/)
    const iso = getState().roles.find((r) => r.name === 'analyst')?.validUntil as string
    expect(field.value.slice(0, 16)).toBe(iso.slice(0, 16))
  })

  it('reads a date the person types as UTC', async () => {
    const writes = recordWrites()
    const { dialog, user } = await editRole('analyst')
    const field = within(dialog).getByLabelText(/password valid until/i)
    await fireEvent.update(field, '2030-02-01T10:00:00')
    await user.click(within(dialog).getByRole('button', { name: /^save role$/i }))
    await waitFor(() => expect(writes.some((w) => w.method === 'PATCH')).toBe(true))
    expect(writes.find((w) => w.method === 'PATCH')?.body).toEqual({
      validUntil: '2030-02-01T10:00:00.000Z',
    })
  })

  it('sends only what was edited, and clearing the date sends null on purpose', async () => {
    const writes = recordWrites()
    const { dialog, user } = await editRole('analyst')
    await fireEvent.update(within(dialog).getByLabelText(/password valid until/i), '')
    await user.click(within(dialog).getByRole('button', { name: /^save role$/i }))
    await waitFor(() => expect(writes.some((w) => w.method === 'PATCH')).toBe(true))
    expect(writes.find((w) => w.method === 'PATCH')?.body).toEqual({ validUntil: null })
  })

  it('refuses an empty connection limit instead of sending an empty string', async () => {
    const writes = recordWrites()
    const { dialog, user } = await editRole('app_rw')
    const limit = within(dialog).getByLabelText(/connection limit/i)
    await user.clear(limit)
    await user.click(within(dialog).getByRole('button', { name: /^save role$/i }))
    expect(await within(dialog).findByText(/whole number/i)).toBeInTheDocument()
    expect(writes).toHaveLength(0)
  })
})
