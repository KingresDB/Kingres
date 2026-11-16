// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.9.0: the Rebalance page against the mock of the real operations (refusals with their problem codes, a
// stop in the middle and a start again, a run that fails, reports with before and after), the replica count dialog
// that follows the task the answer names, a nullable lease holder, and the Ledger page that keeps ?db= on its first load.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { endpoints } from '@/api/endpoints'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { mockConfig } from '@/mocks/state'
import { renderApp } from './helpers'

async function addServer(): Promise<void> {
  const task = await endpoints.addServer({
    address: '10.0.0.14:8091',
    setupToken: 'demo-setup-token',
    allowUnpinned: true,
  })
  await waitFor(async () => expect((await endpoints.getTask(task.id)).state).toBe('done'))
}

describe('a refused start says why, with the problem code', () => {
  const cases: [string, RegExp][] = [
    ['nothing_to_rebalance', /nothing to rebalance/i],
    ['cluster_inconsistent', /cluster is not consistent/i],
    ['version_mismatch', /run different versions/i],
    ['failover_in_progress', /failover is in progress/i],
    ['no_receiving_server', /no server can receive buckets/i],
    ['operation_in_progress', /operation in progress/i],
  ]
  for (const [code, heading] of cases) {
    it(code, async () => {
      const user = userEvent.setup()
      await renderApp('/rebalance')
      await addServer()
      applySwitches(new URLSearchParams(`rebalance=refuse:${code}`), getState())
      await user.click(await screen.findByRole('button', { name: /start rebalance/i }))
      expect((await screen.findAllByText(heading)).length).toBeGreaterThan(0)
      expect(getState().rebalance.state).toBe('idle')
    })
  }

  it('refuses a start when the plan is empty', async () => {
    const user = userEvent.setup()
    await renderApp('/rebalance')
    await user.click(await screen.findByRole('button', { name: /start rebalance/i }))
    expect(await screen.findByText(/nothing to rebalance/i)).toBeInTheDocument()
  })
})

describe('stop in the middle, start again', () => {
  it('stops a run, shows who stopped it, goes on with the same task and writes the report with before and after', async () => {
    const user = userEvent.setup()
    await renderApp('/rebalance')
    await addServer()
    mockConfig.taskDurationMs = 3000
    await user.click(await screen.findByRole('button', { name: /start rebalance/i }))
    // One move at a time, with its phase and the servers.
    const inFlight = await screen.findByRole('table', { name: /moves in flight/i })
    expect(within(inFlight).getAllByRole('row').length).toBe(2)
    const id = getState().rebalance.taskId

    await user.click(await screen.findByRole('button', { name: /stop rebalance/i }))
    const dialog = await screen.findByRole('dialog', { name: /stop the rebalance/i })
    await user.click(within(dialog).getByRole('button', { name: /^stop rebalance$/i }))
    expect(await screen.findByTestId('stopping-note')).toBeInTheDocument()
    const note = await screen.findByTestId('stopped-note', {}, { timeout: 5000 })
    expect(note).toHaveTextContent(/stopped by an operator/i)
    // A stopped run is a report too, listed as stopped; state is never by colour alone.
    expect(await screen.findByText('Stopped')).toBeInTheDocument()

    await user.click(await screen.findByRole('button', { name: /start again/i }))
    await waitFor(() => expect(getState().rebalance.state).toBe('running'))
    expect(getState().rebalance.taskId).toBe(id)
    await waitFor(() => expect(getState().rebalance.state).toBe('idle'), { timeout: 8000 })
    const report = getState().rebalance.reports[0]!
    expect(report.id).toBe(id)
    expect(report.outcome).toBe('completed')
    expect(report.placementAfter!.some((p) => p.serverId === '10.0.0.14')).toBe(true)
    // The report dialog shows before and after.
    await user.click(await screen.findByRole('button', { name: `#${id}` }))
    const placement = await screen.findByTestId('report-placement')
    expect(within(placement).getByText('10.0.0.14')).toBeInTheDocument()
  })

  it('a stopped run can be ended for good: it is recorded as cancelled, not failed', async () => {
    const user = userEvent.setup()
    await renderApp('/rebalance')
    await addServer()
    mockConfig.taskDurationMs = 3000
    await user.click(await screen.findByRole('button', { name: /start rebalance/i }))
    await screen.findByRole('table', { name: /moves in flight/i })
    const id = getState().rebalance.taskId
    await user.click(await screen.findByRole('button', { name: /stop rebalance/i }))
    const stop = await screen.findByRole('dialog', { name: /stop the rebalance/i })
    await user.click(within(stop).getByRole('button', { name: /^stop rebalance$/i }))
    await screen.findByTestId('stopped-note', {}, { timeout: 5000 })

    // The task of a stopped run says that it can be cancelled.
    expect((await endpoints.getTask(id!)).cancellable).toBe(true)
    await user.click(await screen.findByTestId('end-run'))
    const dialog = await screen.findByRole('dialog', { name: /end this run for good/i })
    await user.click(within(dialog).getByRole('button', { name: /^end this run$/i }))
    await waitFor(() => expect(getState().rebalance.state).toBe('idle'))
    const report = getState().rebalance.reports[0]!
    expect(report.id).toBe(id)
    expect(report.outcome).toBe('cancelled')
    expect(report.stoppedBy).toBe('operator')
    expect((await endpoints.getTask(id!)).error?.code).toBe('cancelled')
  })

  it('the report dialog survives an outcome it does not know and a bucket that moved twice', async () => {
    const user = userEvent.setup()
    await renderApp('/rebalance')
    await addServer()
    mockConfig.taskDurationMs = 1500
    await user.click(await screen.findByRole('button', { name: /start rebalance/i }))
    await waitFor(() => expect(getState().rebalance.state).toBe('idle'), { timeout: 8000 })
    const report = getState().rebalance.reports[0]!
    const first = report.moves[0]!
    // The same bucket twice in one run, and an outcome a newer server may send.
    report.moves.push({ ...first }, { ...first, outcome: 'weird' as never })
    await user.click(await screen.findByRole('button', { name: `#${report.id}` }))
    const moves = await screen.findByTestId('report-moves')
    expect(within(moves).getByText('weird')).toBeInTheDocument()
  })

  it('a stop is refused with its code when nothing runs', async () => {
    await renderApp('/rebalance')
    await expect(endpoints.stopRebalance()).rejects.toMatchObject({ code: 'rebalance_not_running' })
  })
})

describe('a run that fails', () => {
  it('shows the failed state, the reason and the bucket that was given up after three tries', async () => {
    const user = userEvent.setup()
    await renderApp('/rebalance')
    await addServer()
    mockConfig.taskDurationMs = 500
    applySwitches(new URLSearchParams('rebalance=fail'), getState())
    await user.click(await screen.findByRole('button', { name: /start rebalance/i }))
    expect(await screen.findByTestId('failed-note', {}, { timeout: 8000 })).toHaveTextContent(
      /given up after 3 attempts/i,
    )
    await user.click(await screen.findByRole('button', { name: /show report/i }))
    const moves = await screen.findByTestId('report-moves')
    expect(within(moves).getAllByText(/failed/i).length).toBeGreaterThan(0)
    expect(within(moves).getAllByText('3').length).toBeGreaterThan(0)
  })
})

describe('the answer to the replica count names the task', () => {
  it('202 with the task for a client that asks for it, 200 for one that does not', async () => {
    await renderApp('/replication')
    await addServer()
    const asked = await endpoints.updateClusterSettingsAsync({ defaultReplicaCount: 0 })
    expect(asked.task?.kind).toBe('standby.reconcile')
    const plain = await endpoints.updateClusterSettings({ defaultReplicaCount: 0 })
    expect(plain.defaultReplicaCount).toBe(0)
  })
})

describe('the Ledger page keeps ?db= on its first load', () => {
  it('opens the database the URL names, not the first one', async () => {
    const s = getState()
    // Two databases use the ledger; the URL names the second.
    const other = s.databases.find((d) => d.info.ledger.mode === 'off')!
    other.info.ledger.mode = 'optIn'
    const names = s.databases.filter((d) => d.info.ledger.mode !== 'off').map((d) => d.info.name)
    expect(names.length).toBeGreaterThan(1)
    const second = names[names.length - 1] as string
    const { router } = await renderApp(`/ledger?db=${second}`)
    await waitFor(() =>
      expect((screen.getByLabelText('Database') as HTMLSelectElement).value).toBe(second),
    )
    expect(router.currentRoute.value.query.db).toBe(second)
  })
})
