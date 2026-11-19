// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// W-13: a task keeps being followed after its dialog is closed and its failure is reported; the
// irreversible actions ask for a typed confirmation.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { mockServer } from '@/mocks/server'
import { getState } from '@/mocks/http'
import { renderApp } from './helpers'

describe('a task whose dialog was closed is still followed (W-13a)', () => {
  it('reports the failure of a hidden drop database in a notice outside the dialog', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/reports')
    await user.click(await screen.findByRole('button', { name: /drop database/i }))
    const dialog = await screen.findByRole('dialog', { name: /drop database reports/i })
    await user.type(within(dialog).getByLabelText(/to confirm/i), 'reports')
    let startedId = 0
    // The task fails only after the dialog was hidden: on a slow machine the click on "Hide" can come after a poll, and a task that
    // already failed in the open dialog is not a hidden one.
    let hidden = false
    mockServer.use(
      http.get('*/api/v1/tasks/:id', ({ params }) => {
        startedId = Number(params.id)
        return HttpResponse.json({
          id: startedId,
          kind: 'database.drop',
          state: hidden ? 'failed' : 'running',
          progress: { percent: 40, currentStep: null, steps: [] },
          error: hidden ? { code: 'x', message: 'the disk is on fire' } : null,
          subject: { type: 'database', id: 'reports' },
          cancellable: false,
          createdAt: '2026-01-01T00:00:00Z',
          updatedAt: '2026-01-01T00:00:00Z',
          createdBy: 'admin',
        })
      }),
    )
    await user.click(within(dialog).getByRole('button', { name: /^drop database$/i }))
    await user.click(await within(dialog).findByRole('button', { name: /hide \(keeps running\)/i }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    hidden = true

    // The notice appears when the next poll of the hidden task sees the failure. On a loaded
    // machine that poll can come later than the default wait of one second.
    const notice = await screen.findByRole(
      'alert',
      { name: /drop database reports/i },
      { timeout: 30_000 },
    )
    expect(notice).toHaveTextContent(/the disk is on fire/i)
    expect(startedId).toBeGreaterThan(0)
    // The notice stays until the person dismisses it.
    await user.click(within(notice).getByRole('button', { name: /dismiss/i }))
    expect(screen.queryByRole('alert', { name: /drop database reports/i })).not.toBeInTheDocument()
  })

  it('tells the person that a hidden task finished', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    await user.click(await screen.findByRole('button', { name: /restore preferred primaries/i }))
    const dialog = await screen.findByRole('dialog', { name: /restore preferred primaries/i })
    await user.click(within(dialog).getByRole('button', { name: /^restore primaries$/i }))
    await user.click(await within(dialog).findByRole('button', { name: /hide \(keeps running\)/i }))
    const notice = await screen.findByRole('status', { name: /restore preferred primaries/i })
    await waitFor(() => expect(notice).toHaveTextContent(/finished/i), { timeout: 30_000 })
  })
})

describe('typed confirmations for irreversible actions (W-13b, W-13c)', () => {
  it('removing a server needs its address typed', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    const row = await screen.findByTestId('server-row-10.0.0.13')
    await user.click(within(row).getByRole('button', { name: /remove/i }))
    const dialog = await screen.findByRole('dialog', { name: /remove server/i })
    const confirm = within(dialog).getByRole('button', { name: /^remove server$/i })
    expect(confirm).toBeDisabled()
    await user.type(within(dialog).getByLabelText(/to confirm/i), '10.0.0.13:8091')
    expect(confirm).toBeEnabled()
  })

  it('a hard failover needs the address typed; a graceful one does not', async () => {
    const { mockConfig } = await import('@/mocks/state')
    // The real server refuses a graceful failover for now; the switch lets the mock accept it.
    mockConfig.failoverGraceful = true
    const user = userEvent.setup()
    await renderApp('/servers')
    const row = await screen.findByTestId('server-row-10.0.0.12')
    await user.click(within(row).getByRole('button', { name: /failover/i }))
    const dialog = await screen.findByRole('dialog', { name: /fail over server/i })
    await user.click(within(dialog).getByRole('button', { name: /show the plan/i }))
    await within(dialog).findByTestId('failover-plan')
    expect(within(dialog).getByRole('button', { name: /graceful failover/i })).toBeEnabled()
    await user.click(within(dialog).getByRole('button', { name: /change the choice/i }))
    await user.click(within(dialog).getByRole('radio', { name: /hard/i }))
    await user.click(within(dialog).getByRole('button', { name: /show the plan/i }))
    await within(dialog).findByTestId('failover-plan')
    const hard = within(dialog).getByRole('button', { name: /hard failover/i })
    expect(hard).toBeDisabled()
    await user.type(within(dialog).getByLabelText(/to confirm/i), '10.0.0.12:8091')
    expect(hard).toBeEnabled()
  })

  it('disabling a role needs its name typed; enabling it again does not', async () => {
    const user = userEvent.setup()
    await renderApp('/security')
    const row = (await screen.findByRole('rowheader', { name: 'app_rw' })).closest(
      'tr',
    ) as HTMLElement
    await user.click(within(row).getByRole('button', { name: /^disable$/i }))
    const dialog = await screen.findByRole('dialog', { name: /disable role app_rw/i })
    expect(getState().roles.find((r) => r.name === 'app_rw')?.canLogin).toBe(true)
    const confirm = within(dialog).getByRole('button', { name: /^disable role$/i })
    expect(confirm).toBeDisabled()
    await user.type(within(dialog).getByLabelText(/to confirm/i), 'app_rw')
    await user.click(confirm)
    await waitFor(() =>
      expect(getState().roles.find((r) => r.name === 'app_rw')?.canLogin).toBe(false),
    )
    const enable = await waitFor(() => {
      const again = screen.getByRole('rowheader', { name: 'app_rw' }).closest('tr') as HTMLElement
      return within(again).getByRole('button', { name: /^enable$/i })
    })
    await user.click(enable)
    await waitFor(() =>
      expect(getState().roles.find((r) => r.name === 'app_rw')?.canLogin).toBe(true),
    )
  })
})
