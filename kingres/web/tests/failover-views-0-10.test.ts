// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The views of the failover console on their own and on the pages (design 8.1, 8.3, 8.5, 8.6): the readiness line of a
// group, the verdict about lost data in words for every verdict, each refusal code with words of its own, the plan, the
// report, the Tasks page and the alerts, what each role sees and does, and hostile text. T20.
import { describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { ApiError } from '@/api/problem'
import { endpoints } from '@/api/endpoints'
import type {
  DataLossAssessment,
  FailoverCandidate,
  FailoverPlan,
  FailoverReadiness,
  FailoverReport,
  ProblemCode,
} from '@/api/types'
import DataLossText from '@/components/DataLossText.vue'
import FailoverPlanTable from '@/components/FailoverPlanTable.vue'
import FailoverReadiness_ from '@/components/FailoverReadiness.vue'
import FailoverRefusal from '@/components/FailoverRefusal.vue'
import FailoverProgress from '@/components/FailoverProgress.vue'
import FailoverReportDialog from '@/components/FailoverReportDialog.vue'
import { getState } from '@/mocks/http'
import { HOSTILE } from '@/mocks/hostile'
import { applySwitches } from '@/mocks/switches'
import { mockConfig } from '@/mocks/state'
import { seedAutomaticFailover } from '@/mocks/failover'
import { createPinia } from 'pinia'
import { renderApp, renderComponent, signInAs } from './helpers'

const loss = (over: Partial<DataLossAssessment> = {}): DataLossAssessment => ({
  verdict: 'none',
  message: 'Server text.',
  lastKnownPrimaryLsn: '0/5000000',
  chosenReceiveLsn: '0/4FFC000',
  lastKnownAgeSeconds: 38,
  groupRanWithoutSynchronousStandby: false,
  asyncDatabases: [],
  unreadableServerIds: [],
  ...over,
})

const candidate = (over: Partial<FailoverCandidate> = {}): FailoverCandidate => ({
  serverId: '10.0.0.13',
  standbyId: 3,
  readable: true,
  eligible: true,
  ineligibleReason: null,
  receiveLsn: '0/5000000',
  replayLsn: '0/5000000',
  lagBytes: 0,
  syncState: 'quorum',
  chosen: true,
  ...over,
})

describe('the verdict about lost data, in words, for every verdict', () => {
  const at = '2026-11-18T12:00:00Z'
  const text = (d: DataLossAssessment, group: number | null = 2) => {
    const { container } = render(DataLossText, { props: { dataLoss: d, groupId: group, at } })
    return container.querySelector('[data-testid="data-loss-text"]')?.textContent ?? ''
  }

  it('none, asyncOnly, possible (without its synchronous standby, behind the best) and unknown', () => {
    expect(text(loss())).toBe('No acknowledged commit can be lost.')
    expect(text(loss({ verdict: 'asyncOnly' }))).toBe(
      'Commits written under asynchronous durability in the last seconds can be lost.',
    )
    // 38 seconds before the plan: "since 18 Nov 2026, 11:59:22 UTC".
    expect(text(loss({ verdict: 'possible', groupRanWithoutSynchronousStandby: true }))).toMatch(
      /^Commits acknowledged since 18 Nov 2026, 11:59:22 UTC can be lost: group 2 ran without its synchronous standby\.$/,
    )
    expect(text(loss({ verdict: 'possible' }))).toMatch(/behind the best one/)
    expect(text(loss({ verdict: 'unknown', unreadableServerIds: ['10.0.0.13'] }))).toBe(
      'The standby on 10.0.0.13 could not be read; commits that only it holds can be lost.',
    )
  })

  it('a verdict the console does not know is said as not known, with the message of the server', () => {
    const { container } = render(DataLossText, {
      props: { dataLoss: loss({ verdict: 'catastrophic' as 'none' }), groupId: 1 },
    })
    expect(container).toHaveTextContent(/not known/i)
    expect(container).toHaveTextContent('Server text.')
  })

  it('is words and a symbol, never colour alone', () => {
    const { container } = render(DataLossText, {
      props: { dataLoss: loss({ verdict: 'possible' }), groupId: 1 },
    })
    const badge = container.querySelector('.badge') as HTMLElement
    expect(badge.textContent).toMatch(/data can be lost/i)
    expect(badge.querySelector('[aria-hidden="true"]')?.textContent).toBeTruthy()
  })

  it('with the numbers: positions, the age of the check, the databases and the unreadable standbys', () => {
    const { container } = render(DataLossText, {
      props: {
        dataLoss: loss({ asyncDatabases: ['shop'], unreadableServerIds: ['10.0.0.13'] }),
        groupId: 1,
        numbers: true,
      },
    })
    const numbers = container.querySelector('[data-testid="data-loss-numbers"]') as HTMLElement
    expect(numbers).toHaveTextContent('0/5000000')
    expect(numbers).toHaveTextContent('0/4FFC000')
    expect(numbers).toHaveTextContent('38s')
    expect(numbers).toHaveTextContent('shop')
    expect(numbers).toHaveTextContent('10.0.0.13')
  })
})

describe('the readiness line of a group', () => {
  const view = (r: FailoverReadiness | undefined) => {
    const { container } = render(FailoverReadiness_, { props: { readiness: r } })
    return container.querySelector('[data-testid="failover-readiness"]') as HTMLElement
  }
  const base: FailoverReadiness = {
    possible: true,
    candidateServerId: '10.0.0.13',
    dataLossVerdict: 'none',
    blockers: [],
    checkedAt: new Date().toISOString(),
  }

  it('possible without loss, with the server of the standby', () => {
    const el = view(base)
    expect(el).toHaveAttribute('data-state', 'possible')
    expect(el).toHaveTextContent('Failover is possible: the standby on 10.0.0.13, no data loss.')
  })

  it('possible, but commits made without a synchronous standby could be lost (a warning)', () => {
    const el = view({ ...base, dataLossVerdict: 'possible' })
    expect(el).toHaveAttribute('data-state', 'possibleLoss')
    expect(el).toHaveTextContent(
      'Failover is possible, but commits made without a synchronous standby could be lost.',
    )
    expect(el.querySelector('.tone-warn')).not.toBeNull()
  })

  it('async only is said as what it is', () => {
    expect(view({ ...base, dataLossVerdict: 'asyncOnly' })).toHaveTextContent(
      /asynchronous durability in the last seconds could be lost/i,
    )
  })

  it('not possible: a cross and the blocker, whatever its code', () => {
    const el = view({
      ...base,
      possible: false,
      candidateServerId: null,
      dataLossVerdict: null,
      blockers: [
        { code: 'no_replica_available', shardGroupId: 2, message: 'Group 2 has no standby.' },
        { code: 'a_code_from_the_future', shardGroupId: null, message: 'Something else.' },
      ],
    })
    expect(el).toHaveAttribute('data-state', 'blocked')
    expect(el).toHaveTextContent('Failover is not possible:')
    const blockers = el.querySelector('[data-testid="readiness-blockers"]') as HTMLElement
    expect(blockers).toHaveTextContent('No standby is ready. Group 2 has no standby.')
    expect(blockers).toHaveTextContent('Blocked. Something else.')
  })

  it('an older server sends nothing, and the card says so', () => {
    expect(view(undefined)).toHaveTextContent(/does not report failover readiness/i)
  })

  it('the Replication page has a line on every group, and it follows the states of the mock', async () => {
    await renderApp('/replication')
    const lines = await screen.findAllByTestId('failover-readiness')
    expect(lines).toHaveLength(4)
    for (const line of lines) expect(line).toHaveAttribute('data-state', 'possible')
    const mock = await import('@/mocks/state')
    mock.mockConfig.failoverReadiness = { 2: 'noStandby', 3: 'possibleLoss', 1: 'noMajority' }
    await waitFor(
      () =>
        expect(
          within(screen.getByTestId('group-2')).getByTestId('failover-readiness'),
        ).toHaveAttribute('data-state', 'blocked'),
      { timeout: 30_000 },
    )
    expect(
      within(screen.getByTestId('group-2')).getByTestId('failover-readiness'),
    ).toHaveTextContent(/no standby is ready\. shard group 2 has no standby/i)
    expect(
      within(screen.getByTestId('group-1')).getByTestId('failover-readiness'),
    ).toHaveTextContent(/no majority of servers/i)
    expect(within(screen.getByTestId('group-3')).getByTestId('failover-readiness')).toHaveAttribute(
      'data-state',
      'possibleLoss',
    )
  })
})

describe('each refusal has words of its own', () => {
  const codes: [ProblemCode, number, RegExp][] = [
    ['failover_needs_decision', 409, /needs your decision/i],
    ['no_replica_available', 409, /no standby is ready/i],
    ['lease_settings_differ', 409, /different lease settings/i],
    ['quorum_unavailable', 503, /no majority of servers/i],
    ['version_mismatch', 409, /run different versions/i],
    ['cluster_inconsistent', 409, /catalog is not consistent/i],
    ['failover_in_progress', 409, /already running/i],
    ['rebalance_in_progress', 409, /a rebalance is running/i],
    ['operation_in_progress', 409, /another operation is running/i],
    ['node_state_invalid', 422, /not possible in this server state/i],
    ['unprocessable', 422, /does not fit the cluster/i],
    ['unsupported_operation', 422, /not available on this server version yet/i],
  ]
  for (const [code, status, heading] of codes) {
    it(`${code}`, () => {
      const error = new ApiError({
        type: 'about:blank',
        title: code,
        status,
        code,
        detail: 'Detail from the server.',
      })
      const { container } = render(FailoverRefusal, {
        props: { error, context: { mode: 'hard', includesMapGroup: false } },
      })
      const notice = container.querySelector('[data-testid="failover-refusal"]') as HTMLElement
      expect(notice).toHaveTextContent(heading)
      // The code itself is never the text, and the server's detail is below the words.
      expect(notice).not.toHaveTextContent(code)
      expect(notice).toHaveTextContent('Detail from the server.')
      expect(notice).toHaveAttribute('data-code', code)
    })
  }

  it('every code has a heading of its own, no two the same', () => {
    const headings = new Set<string>()
    for (const [code, status] of codes) {
      const { container } = render(FailoverRefusal, {
        props: {
          error: new ApiError({ type: 'about:blank', title: code, status, code }),
          context: { mode: 'graceful', includesMapGroup: true },
        },
      })
      headings.add(container.querySelector('strong')?.textContent ?? '')
    }
    expect(headings.size).toBe(codes.length)
  })

  it('a code without words of its own falls back to the general notice', () => {
    const { container } = render(FailoverRefusal, {
      props: {
        error: new ApiError({ type: 'about:blank', title: 'x', status: 403, code: 'forbidden' }),
        context: { mode: 'hard', includesMapGroup: false },
      },
    })
    expect(container.querySelector('[data-testid="failover-refusal"]')).toBeNull()
    expect(container.querySelector('[data-code="forbidden"]')).not.toBeNull()
  })

  it('the unsupported answer says what is not available: graceful and the map group separately', () => {
    const error = new ApiError({
      type: 'about:blank',
      title: 'u',
      status: 422,
      code: 'unsupported_operation',
    })
    const only = (mode: 'hard' | 'graceful', includesMapGroup: boolean) =>
      render(FailoverRefusal, { props: { error, context: { mode, includesMapGroup } } }).container
        .textContent ?? ''
    expect(only('graceful', false)).toMatch(/planned switchover/i)
    expect(only('graceful', false)).not.toMatch(/map group/i)
    expect(only('hard', true)).toMatch(/map group/i)
    expect(only('hard', true)).toMatch(/manager leader/i)
    expect(only('hard', true)).not.toMatch(/planned switchover/i)
  })
})

describe('the plan table', () => {
  const plan = (over: Partial<FailoverPlan> = {}): FailoverPlan => ({
    generatedAt: '2026-11-18T12:00:00Z',
    kind: 'failover',
    mode: 'hard',
    serverId: '10.0.0.12',
    canProceed: true,
    blockers: [],
    willStopRebalance: false,
    groups: [
      {
        shardGroupId: 0,
        isMapGroup: true,
        fromServerId: '10.0.0.12',
        toServerId: '10.0.0.11',
        candidates: [
          candidate({ serverId: '10.0.0.11' }),
          candidate({
            serverId: '10.0.0.13',
            chosen: false,
            eligible: false,
            ineligibleReason: 'Its timeline differs.',
            lagBytes: 4096,
            syncState: null,
          }),
          candidate({ serverId: '10.0.0.14', chosen: false, readable: false, receiveLsn: null }),
        ],
        dataLoss: loss(),
        estimatedSeconds: 30,
        writesPausedSeconds: 3,
        steps: ['Freeze the group', 'Promote the standby'],
        movesTheManager: true,
      },
    ],
    ...over,
  })

  it('says the manager moves, the candidates with their results in text, the times and the steps', () => {
    const { container } = render(FailoverPlanTable, { props: { plan: plan() } })
    const el = container.querySelector('[data-testid="plan-group-0"]') as HTMLElement
    expect(el).toHaveTextContent(/moves the manager of the cluster/i)
    expect(el).toHaveTextContent('Chosen')
    expect(el).toHaveTextContent('Not eligible')
    expect(el).toHaveTextContent('Its timeline differs.')
    expect(el).toHaveTextContent('could not be read')
    expect(el).toHaveTextContent('not streaming')
    expect(el.querySelector('[data-testid="plan-writes-paused"]')).toHaveTextContent('3s')
    expect(el).toHaveTextContent('Steps (2)')
  })

  it('states that a running rebalance will be stopped first', () => {
    const { container } = render(FailoverPlanTable, {
      props: { plan: plan({ willStopRebalance: true }) },
    })
    expect(container.querySelector('[data-testid="plan-stops-rebalance"]')).toHaveTextContent(
      'The running rebalance will be stopped first.',
    )
  })

  it('lists the blockers as messages and says which wait for a decision', () => {
    const { container } = render(FailoverPlanTable, {
      props: {
        plan: plan({
          canProceed: false,
          blockers: [
            { code: 'failoverNeedsDecision', shardGroupId: 0, message: 'Group 0 may lose data.' },
          ],
        }),
      },
    })
    expect(container.querySelector('[data-testid="plan-blockers"]')).toHaveTextContent(
      /waits for your decision.*needs your decision\. Group 0 may lose data\./i,
    )
  })

  it('a plan with no group says so', () => {
    const { container } = render(FailoverPlanTable, { props: { plan: plan({ groups: [] }) } })
    expect(container.querySelector('[data-testid="plan-empty"]')).toBeInTheDocument()
  })
})

describe('the Tasks page and the alerts', () => {
  it('a finished failover has a report button and its steps come in a block per group', async () => {
    const user = userEvent.setup()
    await renderApp('/tasks')
    const task = await endpoints.failoverServer('10.0.0.12', 'hard')
    await waitFor(async () => expect((await endpoints.getTask(task.id)).state).toBe('done'))
    const button = await screen.findByTestId('task-report', {}, { timeout: 30_000 })
    await user.click(button)
    const report = await screen.findByRole('dialog', { name: /failover report/i })
    expect(await within(report).findByTestId('report-outcome')).toHaveTextContent(/completed/i)
    await user.click(within(report).getByRole('button', { name: /close dialog/i }))
    await user.click(screen.getByRole('button', { name: /^details$/i }))
    const dialog = await screen.findByRole('dialog', { name: new RegExp(`task ${task.id}`, 'i') })
    expect(within(dialog).getByTestId('step-group')).toHaveTextContent('Group 2')
    expect(within(dialog).getByTestId('detail-report')).toBeInTheDocument()
  })

  it('a task that is not a failover has no report button, and a failover that runs has none yet', async () => {
    mockConfig.taskDurationMs = 60_000
    const task = await (async () => {
      await renderApp('/tasks')
      return endpoints.failoverServer('10.0.0.12', 'hard')
    })()
    await screen.findAllByText(/fail over a server/i)
    expect(screen.queryByTestId('task-report')).not.toBeInTheDocument()
    expect(task.kind).toBe('server.failover')
  })

  it('an alert about a failover links to the progress and the report, and to its group', async () => {
    applySwitches(new URLSearchParams('failover=needs-decision'), getState())
    await signInAs(createPinia())
    const task = await endpoints.failoverServer('10.0.0.12', 'hard')
    await waitFor(async () => expect((await endpoints.getTask(task.id)).state).toBe('paused'))
    await renderApp('/alerts')
    const alerts = await screen.findAllByText(
      /a failover waits for a decision/i,
      {},
      { timeout: 30_000 },
    )
    const alert = alerts[0] as HTMLElement
    const row = alert.closest('tr, li, article, section') as HTMLElement
    const link = within(row).getByRole('link', { name: /failover progress and report/i })
    expect(link).toHaveAttribute('href', expect.stringContaining('/replication#failover-top'))
    expect(within(row).getByRole('link', { name: /shard group 2/i })).toBeInTheDocument()
  })
})

describe('what each role sees and does (8.6)', () => {
  it('read-only: sees the readiness, the progress and the reports; every action is disabled with the reason', async () => {
    mockConfig.taskDurationMs = 60_000
    seedAutomaticFailover(getState(), '10.0.0.12')
    await renderApp('/replication', { user: 'viewer' })
    const card = await screen.findByTestId('failover-progress')
    expect(await screen.findAllByTestId('failover-readiness')).toHaveLength(4)
    const cancel = within(card).getByTestId('cancel-failover')
    expect(cancel).toHaveAttribute('aria-disabled', 'true')
    expect(cancel).toHaveTextContent(/requires the kingres_ops cluster role/i)
    // The report is read-only information.
    expect(within(card).getByTestId('open-report')).toBeEnabled()
  })

  it('read-only: the Failover button of a server is disabled with the reason', async () => {
    await renderApp('/servers', { user: 'viewer' })
    const row = await screen.findByTestId('server-row-10.0.0.12')
    const button = within(row).getByRole('button', { name: /failover/i })
    expect(button).toHaveAttribute('aria-disabled', 'true')
    expect(button).toHaveTextContent(/requires the kingres_ops/i)
    await userEvent.setup().click(button)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('operations: can start, cancel and decide', async () => {
    const user = userEvent.setup()
    await renderApp('/servers', { user: 'ops' })
    const row = await screen.findByTestId('server-row-10.0.0.12')
    const button = within(row).getByRole('button', { name: /failover/i })
    expect(button).not.toHaveAttribute('aria-disabled')
    await user.click(button)
    expect(await screen.findByRole('dialog', { name: /fail over server/i })).toBeInTheDocument()
  })

  it('operations: Accept data loss and Cancel of a paused run are enabled', async () => {
    applySwitches(new URLSearchParams('failover=needs-decision'), getState())
    await signInAs(createPinia())
    await endpoints.failoverServer('10.0.0.12', 'hard')
    await renderApp('/servers', { user: 'ops' })
    const panel = await screen.findByTestId('waiting-for-you', {}, { timeout: 30_000 })
    await waitFor(() =>
      expect(within(panel).getByTestId('accept-and-continue')).not.toHaveAttribute('aria-disabled'),
    )
    expect(within(panel).getByTestId('cancel-failover')).not.toHaveAttribute('aria-disabled')
  })

  it('a stats account does not read the status at all and sees nothing of the failover', async () => {
    const spy = vi.spyOn(endpoints, 'getFailoverStatus')
    seedAutomaticFailover(getState(), '10.0.0.12')
    const { container } = await renderComponent(FailoverProgress, {}, { user: 'metrics' })
    await new Promise((r) => setTimeout(r, 100))
    expect(spy).not.toHaveBeenCalled()
    expect(container.querySelector('[data-testid="failover-progress"]')).toBeNull()
    spy.mockRestore()
  })
})

describe('hostile text is text', () => {
  const RLO = String.fromCodePoint(0x202e)

  it('the plan: server ids, blockers and reasons from the server are never markup', async () => {
    const hostile = { ...HOSTILE }
    const { container } = render(FailoverPlanTable, {
      props: {
        plan: {
          generatedAt: '2026-11-18T12:00:00Z',
          kind: 'failover',
          mode: 'hard',
          serverId: hostile.markup,
          canProceed: false,
          blockers: [{ code: 'x', shardGroupId: 1, message: hostile.breakout }],
          willStopRebalance: false,
          groups: [
            {
              shardGroupId: 1,
              isMapGroup: false,
              fromServerId: hostile.markup,
              toServerId: `${RLO}nimda`,
              candidates: [
                candidate({
                  serverId: hostile.breakout,
                  eligible: false,
                  ineligibleReason: hostile.newline,
                  chosen: false,
                }),
              ],
              dataLoss: loss({ message: hostile.markup, unreadableServerIds: [hostile.breakout] }),
              estimatedSeconds: 1,
              writesPausedSeconds: null,
              steps: [hostile.markup],
              movesTheManager: false,
            },
          ],
        },
      },
    })
    expect(container.querySelector('img, svg, script')).toBeNull()
    expect((window as unknown as { __pwn?: number }).__pwn).toBeUndefined()
    // The reversing character is shown as a visible escape, not obeyed.
    expect(container.textContent).not.toContain(RLO)
    expect(container.textContent).toContain('<img src=x onerror=window.__pwn=1>')
  })

  it('the report: every string of it, in the dialog', async () => {
    const group: FailoverReport['groups'][number] = {
      shardGroupId: 2,
      outcome: 'failed',
      skipReason: HOSTILE.markup,
      fromServerId: HOSTILE.markup,
      toServerId: `${RLO}nimda`,
      termBefore: 1,
      termAfter: 2,
      timeline: 3,
      leaseWaitMs: 1000,
      durationSeconds: 1,
      candidates: [
        candidate({
          serverId: HOSTILE.breakout,
          ineligibleReason: HOSTILE.markup,
          eligible: false,
          chosen: false,
        }),
      ],
      dataLoss: loss({
        verdict: 'unknown',
        message: HOSTILE.markup,
        unreadableServerIds: [HOSTILE.breakout],
      }),
      unreachedServerIds: [HOSTILE.markup],
      incidents: [{ kind: HOSTILE.breakout, message: HOSTILE.newline }],
      oldPrimary: {
        state: 'failed',
        method: null,
        divergenceBytes: null,
        rewindBytes: null,
        message: HOSTILE.markup,
      },
      error: HOSTILE.breakout,
    }
    const report: FailoverReport = {
      id: 77,
      kind: 'failover',
      trigger: 'manual',
      serverId: HOSTILE.markup,
      outcome: 'failed',
      startedAt: '2026-11-18T12:00:00Z',
      finishedAt: '2026-11-18T12:00:05Z',
      requestedBy: HOSTILE.reversed,
      totals: {
        groupsTotal: 1,
        groupsDone: 0,
        groupsFailed: 1,
        groupsSkipped: 0,
        durationSeconds: 5,
      },
      dataLossPossible: true,
      error: HOSTILE.markup,
      mode: 'hard',
      options: { mode: 'hard', comment: HOSTILE.breakout },
      groups: [group],
      events: [
        {
          at: '2026-11-18T12:00:01Z',
          shardGroupId: 2,
          step: HOSTILE.markup,
          kind: HOSTILE.breakout,
          message: HOSTILE.newline,
        },
      ],
    }
    getState().failover.reports.unshift(report)
    await renderComponent(FailoverReportDialog, { reportId: 77 })
    const dialog = await screen.findByRole('dialog', { name: /failover report #77/i })
    await within(dialog).findByTestId('report-outcome')
    expect(dialog.querySelector('img, svg, script')).toBeNull()
    expect((window as unknown as { __pwn?: number }).__pwn).toBeUndefined()
    expect(dialog.textContent).not.toContain(RLO)
    expect(dialog.textContent).toContain('<img src=x onerror=window.__pwn=1>')
    // A line break in a message is shown as a mark, not as a second line of the report.
    expect(dialog.textContent).not.toContain('\n2026-11-14 12:00:00 ERROR fake second log line')
  })

  it('the progress card: the server, the message of the group and the error', async () => {
    mockConfig.taskDurationMs = 60_000
    applySwitches(new URLSearchParams('failover=fail:promote'), getState())
    mockConfig.taskDurationMs = 60
    seedAutomaticFailover(getState(), '10.0.0.12')
    const rec = getState().tasks.find((t) => t.task.kind === 'server.failover')
    await waitFor(() => expect(rec?.task.state).not.toBe('running'), { timeout: 30_000 }).catch(
      () => undefined,
    )
    await renderApp('/servers')
    await new Promise((r) => setTimeout(r, 150))
    const t = getState().tasks.find((x) => x.task.kind === 'server.failover')
    if (t?.task.error) t.task.error.message = HOSTILE.markup
    if (t?.task.subject) t.task.subject.id = HOSTILE.breakout
    const card = await screen.findByTestId('failover-progress', {}, { timeout: 30_000 })
    expect(card.querySelector('img, svg, script')).toBeNull()
    expect((window as unknown as { __pwn?: number }).__pwn).toBeUndefined()
  })
})
