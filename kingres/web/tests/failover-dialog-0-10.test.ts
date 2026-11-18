// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The failover dialog (design 8.2) against the mock: choosing the mode, the groups and the standby, the plan with the
// verdict about lost data in words, the decision that sets `allowDataLoss`, the typed address, the refusals of the dry
// run and of the start in words of their own, the keyboard and the focus. T20.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { mockConfig, silenceServer } from '@/mocks/state'
import { renderApp } from './helpers'

type User = ReturnType<typeof userEvent.setup>

/** Opens the failover dialog of a server from the Servers page. */
async function openDialog(user: User, id = '10.0.0.12'): Promise<HTMLElement> {
  const row = await screen.findByTestId(`server-row-${id}`)
  await user.click(within(row).getByRole('button', { name: /fail ?over/i }))
  return screen.findByRole('dialog', { name: /fail over server/i })
}

async function showPlan(user: User, dialog: HTMLElement): Promise<void> {
  await user.click(within(dialog).getByRole('button', { name: /^show the plan$/i }))
  await within(dialog).findByTestId('failover-plan')
}

const hard = (dialog: HTMLElement) => within(dialog).getByRole('radio', { name: /^hard/i })
const graceful = (dialog: HTMLElement) => within(dialog).getByRole('radio', { name: /^graceful/i })

describe('the choice', () => {
  it('a live server defaults to graceful, a dead one to hard, and the button says what it is', async () => {
    const user = userEvent.setup()
    silenceServer(getState(), '10.0.0.13', Date.now() - mockConfig.failureMs - 1000)
    await renderApp('/servers')
    let dialog = await openDialog(user, '10.0.0.12')
    expect(graceful(dialog)).toBeChecked()
    await user.click(within(dialog).getByRole('button', { name: /^cancel$/i }))

    // A dead server: the button is the primary action and reads "Fail over".
    const row = await screen.findByTestId('server-row-10.0.0.13')
    const button = within(row).getByRole('button', { name: /^fail over$/i })
    expect(button).toHaveClass('btn-primary')
    await user.click(button)
    dialog = await screen.findByRole('dialog', { name: /fail over server/i })
    expect(hard(dialog)).toBeChecked()
  })

  it('lists the groups the server leads, marks the map group and offers the standbys with their lag', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    const dialog = await openDialog(user, '10.0.0.11')
    const groups = await within(dialog).findByTestId('failover-groups')
    const map = within(groups).getByTestId('choice-0')
    expect(map).toHaveTextContent(/moves the manager of the cluster/i)
    expect(within(groups).getByTestId('choice-1')).not.toHaveTextContent(/manager/i)
    const select = within(map).getByLabelText(/standby to promote in map group/i)
    expect(within(select).getByRole('option', { name: /holds the most wal/i })).toBeInTheDocument()
    expect(
      within(select).getByRole('option', { name: /10\.0\.0\.12: .* behind, quorum/i }),
    ).toBeInTheDocument()
  })

  it('a server that leads no group says so and cannot go on', async () => {
    const user = userEvent.setup()
    const s = getState()
    for (const g of s.groups) if (g.primaryServerId === '10.0.0.13') g.primaryServerId = '10.0.0.11'
    await renderApp('/servers')
    const dialog = await openDialog(user, '10.0.0.13')
    expect(await within(dialog).findByTestId('no-groups')).toHaveTextContent(
      /nothing to fail over/i,
    )
    expect(within(dialog).getByRole('button', { name: /show the plan/i })).toBeDisabled()
  })

  it('the standby that is picked is what the plan asks for', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    const dialog = await openDialog(user, '10.0.0.12')
    await user.click(hard(dialog))
    const select = await within(dialog).findByLabelText(/standby to promote in group 2/i)
    await user.selectOptions(select, '10.0.0.13')
    await showPlan(user, dialog)
    const group = within(dialog).getByTestId('plan-group-2')
    expect(within(group).getByTestId('candidate-chosen').closest('tr')).toHaveTextContent(
      '10.0.0.13',
    )
  })

  it('a group that is unticked is not sent', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    const dialog = await openDialog(user, '10.0.0.11')
    await user.click(hard(dialog))
    // The map group is refused by the real server for now; leaving it out is the way.
    await user.click(within(within(dialog).getByTestId('choice-0')).getByRole('checkbox'))
    await showPlan(user, dialog)
    expect(within(dialog).queryByTestId('plan-group-0')).not.toBeInTheDocument()
    expect(within(dialog).getByTestId('plan-group-1')).toBeInTheDocument()
  })
})

describe('the plan', () => {
  it('shows the move, the candidates with their positions and the verdict in words', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    const dialog = await openDialog(user, '10.0.0.12')
    await user.click(hard(dialog))
    await showPlan(user, dialog)
    const group = within(dialog).getByTestId('plan-group-2')
    expect(group).toHaveTextContent('10.0.0.12')
    expect(group).toHaveTextContent('10.0.0.13')
    expect(within(group).getByTestId('data-loss-text')).toHaveTextContent(
      'No acknowledged commit can be lost.',
    )
    // A position is text, and the chosen one is said in words, not by colour.
    const table = within(group).getByRole('table', {
      name: /standbys of group 2 that could be promoted/i,
    })
    expect(within(table).getByText('0/5000000')).toBeInTheDocument()
    expect(within(table).getByText('Chosen')).toBeInTheDocument()
    expect(within(group).getByText(/steps \(9\)/i)).toBeInTheDocument()
    expect(within(dialog).queryByTestId('accept-loss')).not.toBeInTheDocument()
  })

  it('"Change the choice" goes back, and a change of the choice makes the plan stale', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    const dialog = await openDialog(user, '10.0.0.12')
    await user.click(hard(dialog))
    await showPlan(user, dialog)
    await user.click(within(dialog).getByTestId('plan-back'))
    expect(within(dialog).queryByTestId('failover-plan')).not.toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: /show the plan/i })).toBeEnabled()
  })

  it('the plan stays while the page refreshes the topology behind the dialog', async () => {
    const user = userEvent.setup()
    const { queryClient } = await renderApp('/servers')
    const dialog = await openDialog(user, '10.0.0.12')
    await user.click(hard(dialog))
    await showPlan(user, dialog)
    await queryClient.invalidateQueries({ queryKey: ['topology'] })
    await new Promise((r) => setTimeout(r, 100))
    expect(within(dialog).getByTestId('failover-plan')).toBeInTheDocument()
  })

  it('a plan that may lose data lists its blocker, asks to accept it, and sends allowDataLoss only then', async () => {
    const user = userEvent.setup()
    applySwitches(new URLSearchParams('failover=dataloss'), getState())
    await renderApp('/servers')
    const dialog = await openDialog(user, '10.0.0.12')
    await user.click(hard(dialog))
    await showPlan(user, dialog)
    const group = within(dialog).getByTestId('plan-group-2')
    expect(within(group).getByTestId('data-loss-text')).toHaveTextContent(
      /commits acknowledged since .* can be lost: group 2 ran without its synchronous standby/i,
    )
    expect(within(dialog).getByTestId('plan-blockers')).toHaveTextContent(
      /waits for your decision/i,
    )
    const start = within(dialog).getByRole('button', { name: /^hard failover$/i })
    await user.type(within(dialog).getByLabelText(/to confirm/i), '10.0.0.12:8091')
    // The address is typed and still the button waits for the decision.
    expect(start).toBeDisabled()
    await user.click(
      within(dialog).getByRole('checkbox', { name: /i accept that data may be lost/i }),
    )
    expect(start).toBeEnabled()
    await user.click(start)
    await within(dialog).findByText('Finished.')
    expect(getState().failover.reports[0]?.options.allowDataLoss).toBe(true)
  })

  it('a hard failover needs the address typed, a graceful one (where the server does it) does not', async () => {
    const user = userEvent.setup()
    mockConfig.failoverGraceful = true
    await renderApp('/servers')
    const dialog = await openDialog(user, '10.0.0.12')
    await showPlan(user, dialog)
    expect(within(dialog).getByRole('button', { name: /^graceful failover$/i })).toBeEnabled()
    expect(within(dialog).getByTestId('plan-writes-paused')).toHaveTextContent('2s')
    await user.click(within(dialog).getByTestId('plan-back'))
    await user.click(hard(dialog))
    await showPlan(user, dialog)
    expect(within(dialog).getByRole('button', { name: /^hard failover$/i })).toBeDisabled()
  })
})

describe('what the server refuses is said in words, not as a code', () => {
  it('graceful: the planned switchover is not available yet (the real server refuses it for now)', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    const dialog = await openDialog(user, '10.0.0.12')
    await user.click(within(dialog).getByRole('button', { name: /^show the plan$/i }))
    const notice = await within(dialog).findByTestId('failover-refusal')
    expect(notice).toHaveAttribute('role', 'status')
    expect(notice).toHaveTextContent(/not available on this server version yet/i)
    expect(notice).toHaveTextContent(/planned switchover/i)
    expect(notice).toHaveTextContent(/nothing was changed/i)
    expect(notice).not.toHaveTextContent('unsupported_operation')
    // The choice stays open, so another mode can be tried.
    await user.click(hard(dialog))
    await showPlan(user, dialog)
  })

  it('the map group: the manager of the cluster cannot be moved yet', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    const dialog = await openDialog(user, '10.0.0.11')
    await user.click(hard(dialog))
    await user.click(within(dialog).getByRole('button', { name: /^show the plan$/i }))
    const notice = await within(dialog).findByTestId('failover-refusal')
    expect(notice).toHaveTextContent(/manager of the cluster/i)
    expect(notice).toHaveTextContent(/choose only the other groups/i)
  })

  it('a group with no standby that is ready blocks the plan and the button', async () => {
    const user = userEvent.setup()
    applySwitches(new URLSearchParams('failover=blocked:noreplica'), getState())
    await renderApp('/servers')
    const dialog = await openDialog(user, '10.0.0.12')
    await user.click(hard(dialog))
    await showPlan(user, dialog)
    expect(within(dialog).getByTestId('plan-blockers')).toHaveTextContent(/no standby is ready/i)
    expect(within(dialog).getByTestId('plan-stuck')).toBeInTheDocument()
    await user.type(within(dialog).getByLabelText(/to confirm/i), '10.0.0.12:8091')
    expect(within(dialog).getByRole('button', { name: /^hard failover$/i })).toBeDisabled()
  })

  for (const [what, words] of [
    ['lease', /lease settings differ/i],
    ['rebalance', /a rebalance is running/i],
  ] as const) {
    it(`blocked: ${what} blocks the plan and the button`, async () => {
      const user = userEvent.setup()
      applySwitches(new URLSearchParams(`failover=blocked:${what}`), getState())
      await renderApp('/servers')
      const dialog = await openDialog(user, '10.0.0.12')
      await user.click(hard(dialog))
      await showPlan(user, dialog)
      expect(within(dialog).getByTestId('plan-blockers')).toHaveTextContent(words)
      expect(within(dialog).getByRole('button', { name: /^hard failover$/i })).toBeDisabled()
    })
  }

  it('no majority: the dry run is refused with its own words', async () => {
    const user = userEvent.setup()
    applySwitches(new URLSearchParams('failover=blocked:quorum'), getState())
    await renderApp('/servers')
    const dialog = await openDialog(user, '10.0.0.12')
    await user.click(hard(dialog))
    await user.click(within(dialog).getByRole('button', { name: /^show the plan$/i }))
    const notice = await within(dialog).findByTestId('failover-refusal')
    expect(notice).toHaveAttribute('role', 'alert')
    expect(notice).toHaveTextContent(/no majority of servers/i)
    expect(notice).toHaveTextContent(/nothing was changed/i)
  })

  for (const [what, words] of [
    ['lease', /the servers use different lease settings/i],
    ['noreplica', /no standby is ready/i],
  ] as const) {
    it(`a refusal of the start itself (${what}) has words and the detail of the server`, async () => {
      const user = userEvent.setup()
      await renderApp('/servers')
      const dialog = await openDialog(user, '10.0.0.12')
      await user.click(hard(dialog))
      await showPlan(user, dialog)
      // The cluster changes between the plan and the start.
      applySwitches(new URLSearchParams(`failover=blocked:${what}`), getState())
      await user.type(within(dialog).getByLabelText(/to confirm/i), '10.0.0.12:8091')
      await user.click(within(dialog).getByRole('button', { name: /^hard failover$/i }))
      const notice = await within(dialog).findByTestId('failover-refusal')
      expect(notice).toHaveTextContent(words)
      expect(within(notice).getByTestId('refusal-detail')).toHaveTextContent(/the server says/i)
      expect(getState().tasks.some((t) => t.task.kind === 'server.failover')).toBe(false)
    })
  }

  it('the plan changed under the person: the 409 asks for the decision and the plan is read again', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    const dialog = await openDialog(user, '10.0.0.12')
    await user.click(hard(dialog))
    await showPlan(user, dialog)
    expect(within(dialog).queryByTestId('accept-loss')).not.toBeInTheDocument()
    // The leader now knows the group ran without its synchronous standby.
    mockConfig.failoverDataLoss = true
    await user.type(within(dialog).getByLabelText(/to confirm/i), '10.0.0.12:8091')
    await user.click(within(dialog).getByRole('button', { name: /^hard failover$/i }))
    const notice = await within(dialog).findByTestId('failover-refusal')
    expect(notice).toHaveTextContent(/needs your decision/i)
    // The 409 carries no plan (the Problem schema has no member for it), so the dialog asked for one.
    const accept = await within(dialog).findByTestId('accept-loss')
    expect(accept).toBeInTheDocument()
    expect(within(dialog).getByTestId('data-loss-text')).toHaveTextContent(/can be lost/i)
    expect(within(dialog).getByRole('button', { name: /^hard failover$/i })).toBeDisabled()
  })
})

describe('keyboard and focus', () => {
  it('focus moves into the dialog, stays in it, Escape closes it and focus returns to the button', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    const row = await screen.findByTestId('server-row-10.0.0.12')
    const opener = within(row).getByRole('button', { name: /failover/i })
    await user.click(opener)
    const dialog = await screen.findByRole('dialog', { name: /fail over server/i })
    await waitFor(() => expect(dialog.contains(document.activeElement)).toBe(true))
    for (let i = 0; i < 25; i += 1) {
      await user.tab()
      expect(dialog.contains(document.activeElement)).toBe(true)
    }
    await user.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(opener).toHaveFocus()
  })

  it('everything is reachable by keyboard: the plan, the decision, the address and the start', async () => {
    const user = userEvent.setup()
    applySwitches(new URLSearchParams('failover=dataloss'), getState())
    await renderApp('/servers')
    const dialog = await openDialog(user, '10.0.0.12')
    await user.click(hard(dialog))
    // Show the plan with the keyboard.
    within(dialog)
      .getByRole('button', { name: /^show the plan$/i })
      .focus()
    await user.keyboard('{Enter}')
    const heading = await within(dialog).findByTestId('plan-heading')
    // The plan is announced by moving focus to its heading.
    await waitFor(() => expect(heading).toHaveFocus())
    const checkbox = within(dialog).getByRole('checkbox', {
      name: /i accept that data may be lost/i,
    })
    checkbox.focus()
    await user.keyboard(' ')
    expect(checkbox).toBeChecked()
    const typed = within(dialog).getByLabelText(/to confirm/i)
    await user.click(typed)
    await user.keyboard('10.0.0.12:8091')
    within(dialog)
      .getByRole('button', { name: /^hard failover$/i })
      .focus()
    await user.keyboard('{Enter}')
    await within(dialog).findByText('Finished.')
  })

  it('a dialog that is closed starts nothing', async () => {
    const user = userEvent.setup()
    await renderApp('/servers')
    await openDialog(user, '10.0.0.12')
    await user.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(getState().tasks.some((t) => t.task.kind === 'server.failover')).toBe(false)
  })
})
