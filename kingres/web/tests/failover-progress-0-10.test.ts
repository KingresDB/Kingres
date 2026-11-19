// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The progress and the report of a failover (design 8.3) against the mock: the steps in one block per group, the phase,
// the cancel that is refused after the promotion, a run that waits for a decision and goes on when it is accepted, a
// failed group, a partly done run, the map group's bootstrap, an automatic run, and the report of each. T20.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { mockConfig } from '@/mocks/state'
import { resetFailoverState } from '@/mocks/failover'
import { endpoints } from '@/api/endpoints'
import { createPinia } from 'pinia'
import { renderApp, signInAs } from './helpers'

type User = ReturnType<typeof userEvent.setup>

/** Starts a hard failover of an ordinary group by hand, through the dialog, and closes the dialog. */
async function startByHand(user: User, id = '10.0.0.12', address = `${id}:8091`): Promise<void> {
  const row = await screen.findByTestId(`server-row-${id}`)
  await user.click(within(row).getByRole('button', { name: /fail ?over/i }))
  const dialog = await screen.findByRole('dialog', { name: /fail over server/i })
  await user.click(within(dialog).getByRole('radio', { name: /^hard/i }))
  await user.click(within(dialog).getByRole('button', { name: /^show the plan$/i }))
  await within(dialog).findByTestId('failover-plan')
  await user.type(within(dialog).getByLabelText(/to confirm/i), address)
  await user.click(within(dialog).getByRole('button', { name: /^hard failover$/i }))
  await within(dialog).findByRole('progressbar')
  await user.click(
    within(dialog).getByRole('button', { name: /^(hide \(keeps running\)|close)$/i }),
  )
  await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
}

/** The task of the run the mock drives. */
const runTask = () => getState().tasks.find((t) => t.task.kind === 'server.failover')

/** Puts a slow run at `fraction` of its way (the mock follows the clock). */
function moveTo(fraction: number): void {
  const rec = runTask()
  if (rec) rec.startedAt = Date.now() - fraction * rec.durationMs
}

describe('a run in progress', () => {
  it('shows the steps in a block per group, the phase, the state of the group and the cancel button', async () => {
    const user = userEvent.setup()
    mockConfig.taskDurationMs = 60_000
    await renderApp('/servers')
    await startByHand(user)
    const card = await screen.findByTestId('failover-progress')
    expect(card).toHaveAttribute('data-state', 'running')
    expect(
      within(card).getByRole('heading', { name: /failover of server 10\.0\.0\.12/i }),
    ).toBeInTheDocument()
    expect(within(card).getByTestId('failover-trigger')).toHaveTextContent(/started by a person/i)
    const group = within(card).getByTestId('fg-2')
    expect(group).toHaveTextContent(/shard group 2|group 2/i)
    expect(group).toHaveTextContent('from 10.0.0.12 to 10.0.0.13')
    // The steps of the task come in a block per group, without the "Group 2: " prefix repeated on every line.
    expect(within(card).getByTestId('step-group')).toHaveTextContent('Group 2')
    expect(within(card).getByText('Freeze the group')).toBeInTheDocument()
    expect(within(card).queryByText(/^Group 2: Freeze/)).not.toBeInTheDocument()
    expect(within(card).getByTestId('cancel-failover')).not.toHaveAttribute('aria-disabled')
    expect(within(card).getByTestId('failover-phase')).toHaveTextContent(/checking|freez/i)
  })

  it('names the lease wait and the phase while it waits', async () => {
    const user = userEvent.setup()
    mockConfig.taskDurationMs = 60_000
    await renderApp('/servers')
    await startByHand(user)
    await screen.findByTestId('failover-progress')
    // Step 4 of 11 (the wait for the old lease) is at 3.5 / 11 of the way.
    moveTo(3.4 / 11)
    const message = await screen.findByTestId('group-message', {}, { timeout: 6000 })
    expect(message).toHaveTextContent(/waiting \d+ s for the old primary's lease to run out/i)
    expect(screen.getByTestId('failover-phase')).toHaveTextContent(/lease/i)
  })

  it('cancel works before the promotion: nothing changes and the report says cancelled', async () => {
    const user = userEvent.setup()
    mockConfig.taskDurationMs = 60_000
    await renderApp('/servers')
    await startByHand(user)
    const card = await screen.findByTestId('failover-progress')
    await user.click(within(card).getByTestId('cancel-failover'))
    await waitFor(() => expect(screen.queryByTestId('failover-progress')).not.toBeInTheDocument(), {
      timeout: 6000,
    })
    expect(runTask()?.task.error?.code).toBe('cancelled')
    expect(getState().groups.find((g) => g.id === 2)?.primaryServerId).toBe('10.0.0.12')
    expect(getState().failover.reports[0]?.outcome).toBe('cancelled')
  })

  it('after the promotion cancel is disabled and says why', async () => {
    const user = userEvent.setup()
    mockConfig.taskDurationMs = 60_000
    await renderApp('/servers')
    await startByHand(user)
    await screen.findByTestId('failover-progress')
    moveTo(7.2 / 11)
    const reason = await screen.findByTestId('cancel-reason', {}, { timeout: 6000 })
    expect(reason).toHaveTextContent(
      /a standby has been promoted; the failover can only go forward/i,
    )
    const cancel = screen.getByTestId('cancel-failover')
    expect(cancel).toHaveAttribute('aria-disabled', 'true')
    await user.click(cancel)
    expect(runTask()?.task.state).toBe('running')
  })

  it('an automatic failover says so', async () => {
    mockConfig.taskDurationMs = 60_000
    applySwitches(new URLSearchParams('failover=auto:10.0.0.12,slow'), getState())
    await renderApp('/servers')
    const card = await screen.findByTestId('failover-progress')
    expect(within(card).getByTestId('failover-trigger')).toHaveTextContent(/automatically/i)
    expect(runTask()?.task.createdBy).toBe('system')
  })

  it('the map group: the manager moves first and the run has no task until the catalog follows', async () => {
    const user = userEvent.setup()
    applySwitches(new URLSearchParams('failover=map'), getState())
    mockConfig.taskDurationMs = 60_000
    await renderApp('/servers')
    const row = await screen.findByTestId('server-row-10.0.0.11')
    await user.click(within(row).getByRole('button', { name: /fail ?over/i }))
    const dialog = await screen.findByRole('dialog', { name: /fail over server/i })
    await user.click(within(dialog).getByRole('radio', { name: /^hard/i }))
    await user.click(within(dialog).getByRole('button', { name: /^show the plan$/i }))
    const map = await within(dialog).findByTestId('plan-group-0')
    expect(map).toHaveTextContent(/moves the manager of the cluster/i)
    await user.type(within(dialog).getByLabelText(/to confirm/i), '10.0.0.11:8091')
    await user.click(within(dialog).getByRole('button', { name: /^hard failover$/i }))
    await within(dialog).findByRole('progressbar')
    await user.click(within(dialog).getByRole('button', { name: /hide/i }))
    await screen.findByTestId('failover-progress')
    moveTo(4 / 22)
    const moving = await screen.findByTestId('manager-moving', {}, { timeout: 6000 })
    expect(moving).toHaveTextContent(/the manager of the cluster is moving to server 10\.0\.0\.12/i)
    expect(screen.getByTestId('no-task-yet')).toBeInTheDocument()
    // There is no task to cancel yet, and the button says so.
    expect(screen.getByTestId('cancel-failover')).toHaveAttribute('aria-disabled', 'true')
    moveTo(15 / 22)
    await waitFor(() => expect(screen.queryByTestId('manager-moving')).not.toBeInTheDocument(), {
      timeout: 6000,
    })
  })
})

describe('a run that waits for a decision', () => {
  async function pause(user: User): Promise<HTMLElement> {
    applySwitches(new URLSearchParams('failover=needs-decision'), getState())
    await renderApp('/servers')
    await startByHand(user)
    return screen.findByTestId('waiting-for-you', {}, { timeout: 6000 })
  }

  it('says what it needs, in words, with the blockers and the verdict of the group', async () => {
    const user = userEvent.setup()
    const panel = await pause(user)
    expect(runTask()?.task.state).toBe('paused')
    expect(within(panel).getByTestId('paused-reason')).toHaveTextContent(/leader restarted/i)
    expect(within(panel).getByTestId('needs-decision')).toHaveTextContent(
      /needs your decision \(group 2\)/i,
    )
    const loss = await within(panel).findByTestId('decision-loss-2')
    expect(loss).toHaveTextContent(/can be lost/i)
    expect(screen.getByTestId('failover-state')).toHaveTextContent(/waiting for you/i)
    expect(
      getState().alerts.some((a) => a.kind === 'failoverNeedsDecision' && a.state === 'open'),
    ).toBe(true)
  })

  it('"Accept data loss and continue" repeats the request with allowDataLoss and the run finishes', async () => {
    const user = userEvent.setup()
    const panel = await pause(user)
    await waitFor(() =>
      expect(within(panel).getByTestId('accept-and-continue')).not.toHaveAttribute('aria-disabled'),
    )
    await user.click(within(panel).getByTestId('accept-and-continue'))
    const confirm = await screen.findByRole('dialog', { name: /continue and accept data loss/i })
    expect(confirm).toHaveTextContent(/not recovered/i)
    await user.click(
      within(confirm).getByRole('button', { name: /accept data loss and continue/i }),
    )
    await waitFor(() => expect(runTask()?.task.state).toBe('done'), { timeout: 6000 })
    const report = getState().failover.reports[0]
    expect(report?.outcome).toBe('completed')
    // The request was built from the options the run was started with, plus the decision.
    expect(report?.options).toMatchObject({ mode: 'hard', shardGroupIds: [2], allowDataLoss: true })
    expect(
      getState().alerts.some((a) => a.kind === 'failoverNeedsDecision' && a.state === 'open'),
    ).toBe(false)
    expect(getState().alerts.some((a) => a.kind === 'failoverDataLoss')).toBe(true)
  })

  it('without the decision a repeat of the request is refused, so nothing goes on by a retry', async () => {
    const user = userEvent.setup()
    await pause(user)
    await expect(endpoints.failoverServer('10.0.0.12', 'hard')).rejects.toMatchObject({
      code: 'failover_needs_decision',
    })
    expect(runTask()?.task.state).toBe('paused')
  })

  it('Cancel ends the paused run without a promotion', async () => {
    const user = userEvent.setup()
    const panel = await pause(user)
    await user.click(within(panel).getByTestId('cancel-failover'))
    await waitFor(() => expect(runTask()?.task.state).toBe('failed'))
    expect(runTask()?.task.error?.code).toBe('cancelled')
    expect(getState().groups.find((g) => g.id === 2)?.primaryServerId).toBe('10.0.0.12')
    expect(getState().failover.reports[0]?.outcome).toBe('cancelled')
  })

  it('the dialog that started it says the run waits, and where to decide', async () => {
    const user = userEvent.setup()
    applySwitches(new URLSearchParams('failover=needs-decision'), getState())
    await renderApp('/servers')
    const row = await screen.findByTestId('server-row-10.0.0.12')
    await user.click(within(row).getByRole('button', { name: /fail ?over/i }))
    const dialog = await screen.findByRole('dialog', { name: /fail over server/i })
    await user.click(within(dialog).getByRole('radio', { name: /^hard/i }))
    await user.click(within(dialog).getByRole('button', { name: /^show the plan$/i }))
    await within(dialog).findByTestId('failover-plan')
    await user.type(within(dialog).getByLabelText(/to confirm/i), '10.0.0.12:8091')
    await user.click(within(dialog).getByRole('button', { name: /^hard failover$/i }))
    expect(
      await within(dialog).findByTestId('failover-paused-note', {}, { timeout: 6000 }),
    ).toHaveTextContent(/waits for you/i)
  })
})

describe('a run that fails, and a run that is partly done', () => {
  it('a failed group: the card says so, with the error and the way to the report', async () => {
    const user = userEvent.setup()
    applySwitches(new URLSearchParams('failover=fail:promote'), getState())
    await renderApp('/servers')
    await startByHand(user)
    const card = await screen.findByTestId('failover-progress', {}, { timeout: 6000 })
    await waitFor(() => expect(card).toHaveAttribute('data-state', 'failed'), { timeout: 6000 })
    expect(within(card).getByTestId('failover-state')).toHaveTextContent(/failed/i)
    expect(within(card).getByTestId('fg-2')).toHaveTextContent(/failed/i)
    expect(
      within(card).getAllByText(/the step "promote the standby" failed/i).length,
    ).toBeGreaterThan(0)
    expect(getState().alerts.some((a) => a.kind === 'failoverBlocked')).toBe(true)
    await user.click(within(card).getByTestId('open-report'))
    const report = await screen.findByRole('dialog', { name: /failover report/i })
    expect(await within(report).findByTestId('report-outcome')).toHaveTextContent(/failed/i)
    expect(within(report).getByTestId('report-group-2')).toHaveAttribute('data-outcome', 'failed')
    expect(within(report).getByTestId('report-group-error')).toHaveTextContent(/failed/i)
    // The promotion did not happen, so the group is still led by the old primary.
    expect(getState().groups.find((g) => g.id === 2)?.primaryServerId).toBe('10.0.0.12')
  })

  it('a run of two groups that fails in the second is partly done: the first stays promoted', async () => {
    applySwitches(new URLSearchParams('failover=map,fail:promote:1'), getState())
    await renderApp('/servers')
    const task = await endpoints.failoverServer('10.0.0.11', 'hard')
    await waitFor(async () => expect((await endpoints.getTask(task.id)).state).toBe('failed'))
    const report = await endpoints.getFailoverReport(task.id)
    expect(report.outcome).toBe('partial')
    expect(report.totals).toMatchObject({ groupsDone: 1, groupsFailed: 1 })
    expect(report.groups.map((g) => [g.shardGroupId, g.outcome])).toEqual([
      [0, 'done'],
      [1, 'failed'],
    ])
    expect(getState().groups.find((g) => g.id === 0)?.primaryServerId).toBe('10.0.0.12')
    expect(getState().groups.find((g) => g.id === 1)?.primaryServerId).toBe('10.0.0.11')
  })

  it('a failure in a later group skips what is after it', async () => {
    applySwitches(new URLSearchParams('failover=map,fail:choose:0'), getState())
    await renderApp('/servers')
    const task = await endpoints.failoverServer('10.0.0.11', 'hard')
    await waitFor(async () => expect((await endpoints.getTask(task.id)).state).toBe('failed'))
    const report = await endpoints.getFailoverReport(task.id)
    expect(report.outcome).toBe('failed')
    expect(report.groups.map((g) => g.outcome)).toEqual(['failed', 'skipped'])
    expect(report.groups[1]?.skipReason).toMatch(/earlier group failed/i)
  })
})

describe('the report of a finished run', () => {
  it('the Replication page names the last failover and opens its report with the numbers', async () => {
    const user = userEvent.setup()
    applySwitches(new URLSearchParams('failover=dataloss,old:rebuilding'), getState())
    const app = await renderApp('/replication')
    const task = await endpoints.failoverServer('10.0.0.12', { mode: 'hard', allowDataLoss: true })
    await waitFor(async () => expect((await endpoints.getTask(task.id)).state).toBe('done'), {
      timeout: 30_000,
    })
    // The page reads the status again at once instead of at its next poll: how long a poll takes is not what is tested.
    await app.queryClient.invalidateQueries()
    const last = await screen.findByTestId('last-failover', {}, { timeout: 30_000 })
    expect(last).toHaveTextContent(/failover of server 10\.0\.0\.12/i)
    await user.click(within(last).getByTestId('last-failover-report'))
    const dialog = await screen.findByRole('dialog', {
      name: new RegExp(`failover report #${task.id}`, 'i'),
    })
    expect(await within(dialog).findByTestId('report-outcome')).toHaveTextContent(/completed/i)
    expect(within(dialog).getByTestId('report-data-loss-possible')).toHaveTextContent(/possible/i)
    const group = within(dialog).getByTestId('report-group-2')
    expect(within(group).getByTestId('report-terms')).toHaveTextContent('4 before, 5 after')
    expect(within(group).getByTestId('report-lease-wait')).toHaveTextContent('10s')
    expect(within(group).getByTestId('data-loss-numbers')).toHaveTextContent('0/4FFC000')
    expect(within(group).getByTestId('report-old-primary')).toHaveTextContent(
      /rebuilding.*pg_rewind.*12\.0 MiB rewound/i,
    )
    expect(within(group).getByRole('table', { name: /candidates of group 2/i })).toBeInTheDocument()
    expect(within(dialog).getByTestId('report-events')).toHaveTextContent(
      /is now served by 10\.0\.0\.13/i,
    )
  })

  it('the state of the old primary in each of its forms, and the alert it raises', async () => {
    await signInAs(createPinia())
    for (const [old, state, words] of [
      ['waiting', 'waitingForServer', /server of the old primary does not answer/i],
      ['failed', 'failed', /base backup failed/i],
      ['running', 'failed', /still runs/i],
    ] as const) {
      applySwitches(new URLSearchParams(`failover=old:${old}`), getState())
      const task = await endpoints.failoverServer('10.0.0.12', 'hard')
      await waitFor(async () => expect((await endpoints.getTask(task.id)).state).toBe('done'))
      const report = await endpoints.getFailoverReport(task.id)
      expect(report.groups[0]?.oldPrimary.state).toBe(state)
      expect(report.groups[0]?.oldPrimary.message).toMatch(words)
      // The next form starts from the cluster as it was; the alerts stay for the check below.
      const alerts = getState().alerts
      resetFailoverState(getState())
      getState().alerts = alerts
    }
    expect(getState().alerts.some((a) => a.kind === 'oldPrimaryRunning')).toBe(true)
    expect(getState().alerts.some((a) => a.kind === 'oldPrimaryNotRebuilt')).toBe(true)
  })

  it('a report that is paused is listed with its outcome', async () => {
    await signInAs(createPinia())
    applySwitches(new URLSearchParams('failover=needs-decision'), getState())
    const task = await endpoints.failoverServer('10.0.0.12', 'hard')
    await waitFor(async () => expect((await endpoints.getTask(task.id)).state).toBe('paused'))
    const page = await endpoints.listFailoverReports()
    expect(page.items[0]).toMatchObject({ id: task.id, outcome: 'paused', finishedAt: null })
    const report = await endpoints.getFailoverReport(task.id)
    expect(report.groups[0]?.outcome).toBe('pending')
    expect(report.groups[0]?.dataLoss.verdict).toBe('possible')
  })
})
