// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The states of the failover console pass axe and log no Vue warning: the dialog in each of its steps, the progress card
// running, waiting for a decision and failed, the report, and the pages that carry them (colour contrast is covered by
// tokens-contrast.test.ts and, in a real browser, by the browser tests). T20.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { configureAxe } from 'vitest-axe'
import { endpoints } from '@/api/endpoints'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { mockConfig } from '@/mocks/state'
import { renderApp, settled } from './helpers'

const axe = configureAxe({ rules: { 'color-contrast': { enabled: false } } })

describe('failover console', () => {
  let errors: ReturnType<typeof vi.spyOn>
  let warnings: ReturnType<typeof vi.spyOn>
  beforeEach(() => {
    errors = vi.spyOn(console, 'error').mockImplementation(() => {})
    warnings = vi.spyOn(console, 'warn').mockImplementation(() => {})
  })
  afterEach(() => {
    errors.mockRestore()
    warnings.mockRestore()
  })
  const clean = async (container: Element) => {
    expect(await axe(container)).toHaveNoViolations()
    expect(errors).not.toHaveBeenCalled()
    expect(warnings).not.toHaveBeenCalled()
  }

  it('the dialog: the choice, a refusal, the plan, the plan with a decision to make, and the progress', async () => {
    const user = userEvent.setup()
    applySwitches(new URLSearchParams('failover=dataloss'), getState())
    await renderApp('/servers')
    const row = await screen.findByTestId('server-row-10.0.0.12')
    await user.click(within(row).getByRole('button', { name: /failover/i }))
    const dialog = await screen.findByRole('dialog', { name: /fail over server/i })
    await within(dialog).findByTestId('failover-groups')
    await clean(dialog)

    // The refusal (graceful is not available on the real server for now).
    await user.click(within(dialog).getByRole('button', { name: /^show the plan$/i }))
    await within(dialog).findByTestId('failover-refusal')
    await clean(dialog)

    await user.click(within(dialog).getByRole('radio', { name: /^hard/i }))
    await user.click(within(dialog).getByRole('button', { name: /^show the plan$/i }))
    await within(dialog).findByTestId('accept-loss')
    await clean(dialog)

    await user.click(
      within(dialog).getByRole('checkbox', { name: /i accept that data may be lost/i }),
    )
    await user.type(within(dialog).getByLabelText(/to confirm/i), '10.0.0.12:8091')
    await user.click(within(dialog).getByRole('button', { name: /^hard failover$/i }))
    await within(dialog).findByRole('progressbar')
    await clean(dialog)
    await within(dialog).findByText('Finished.')
    await clean(dialog)
  })

  it('the progress card running, with its groups and steps', async () => {
    mockConfig.taskDurationMs = 60_000
    const { container } = await renderApp('/servers')
    await endpoints.failoverServer('10.0.0.12', 'hard')
    await screen.findByTestId('failover-progress', {}, { timeout: 30_000 })
    await settled()
    await clean(container)
  }, 90_000)

  it('the progress card waiting for a decision, and the confirmation of the decision', async () => {
    const user = userEvent.setup()
    applySwitches(new URLSearchParams('failover=needs-decision'), getState())
    const { container } = await renderApp('/servers')
    await endpoints.failoverServer('10.0.0.12', 'hard')
    const panel = await screen.findByTestId('waiting-for-you', {}, { timeout: 30_000 })
    await within(panel).findByTestId('decision-loss-2')
    await waitFor(() =>
      expect(within(panel).getByTestId('accept-and-continue')).not.toHaveAttribute('aria-disabled'),
    )
    await clean(container)
    await user.click(within(panel).getByTestId('accept-and-continue'))
    const confirm = await screen.findByRole('dialog', { name: /continue and accept data loss/i })
    await clean(confirm)
  }, 90_000)

  it('the progress card of a run that failed, and the report that it opens', async () => {
    const user = userEvent.setup()
    applySwitches(new URLSearchParams('failover=fail:promote'), getState())
    const { container } = await renderApp('/servers')
    await endpoints.failoverServer('10.0.0.12', 'hard')
    const card = await screen.findByTestId('failover-progress', {}, { timeout: 30_000 })
    await waitFor(() => expect(card).toHaveAttribute('data-state', 'failed'), { timeout: 30_000 })
    await clean(container)
    await user.click(within(card).getByTestId('open-report'))
    const report = await screen.findByRole('dialog', { name: /failover report/i })
    await within(report).findByTestId('report-outcome')
    await clean(report)
  }, 90_000)

  it('the report of a finished run with every part of it', async () => {
    const user = userEvent.setup()
    applySwitches(new URLSearchParams('failover=dataloss,old:rebuilding'), getState())
    await renderApp('/replication')
    const task = await endpoints.failoverServer('10.0.0.12', { mode: 'hard', allowDataLoss: true })
    await waitFor(async () => expect((await endpoints.getTask(task.id)).state).toBe('done'))
    const last = await screen.findByTestId('last-failover', {}, { timeout: 30_000 })
    await user.click(within(last).getByTestId('last-failover-report'))
    const report = await screen.findByRole('dialog', { name: /failover report/i })
    await within(report).findByTestId('report-old-primary')
    await clean(report)
  }, 90_000)

  it('the Replication page with a line on every group, a run and a last failover', async () => {
    applySwitches(new URLSearchParams('failover=dataloss'), getState())
    const { container } = await renderApp('/replication')
    await screen.findAllByTestId('failover-readiness')
    const task = await endpoints.failoverServer('10.0.0.12', { mode: 'hard', allowDataLoss: true })
    await waitFor(async () => expect((await endpoints.getTask(task.id)).state).toBe('done'))
    await screen.findByTestId('last-failover', {}, { timeout: 30_000 })
    await settled()
    await clean(container)
  }, 90_000)

  it('the Tasks page with a failover and its report button', async () => {
    const { container, ...app } = await renderApp('/tasks')
    const task = await endpoints.failoverServer('10.0.0.12', 'hard')
    await waitFor(async () => expect((await endpoints.getTask(task.id)).state).toBe('done'), {
      timeout: 30_000,
    })
    await app.queryClient.invalidateQueries()
    await screen.findByTestId('task-report', {}, { timeout: 30_000 })
    await settled()
    await clean(container)
  }, 90_000)
})
