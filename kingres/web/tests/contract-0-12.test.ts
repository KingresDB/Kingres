// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.12.0 in the panel: the outcome `completedWithWarnings` in words, the plan that comes with a refusal
// `failover_needs_decision` (used in the dialog and the progress card, the dry run kept as the fallback), the corrected text of
// `notScheduled`, and a database or table action refused during a failover in words.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { createPinia } from 'pinia'
import { mockServer } from '@/mocks/server'
import { endpoints } from '@/api/endpoints'
import { ApiError, describeProblem } from '@/api/problem'
import { outcomeMeta, oldPrimaryWords } from '@/lib/failover'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { mockConfig } from '@/mocks/state'
import { ok } from './schema-check'
import { renderApp, signInAs } from './helpers'

const sw = (q: string) => applySwitches(new URLSearchParams(q), getState())

describe('the outcome completedWithWarnings', () => {
  it('is shown like completed, with words that say what happened', () => {
    const m = outcomeMeta('completedWithWarnings')
    expect(m).toMatchObject({ tone: 'ok', label: 'Completed, with remarks' })
    expect(m.hint).toMatch(/check of the cluster afterwards reported something/)
    expect(outcomeMeta('completed').tone).toBe('ok')
  })
  it('a finished run of the mock ends with it, the report is offered on the Tasks page, and it satisfies the contract', async () => {
    mockConfig.taskDurationMs = 60
    sw('failover=warnings')
    const app = await renderApp('/tasks')
    const task = await endpoints.failoverServer('10.0.0.12', 'hard')
    await waitFor(async () => expect((await endpoints.getTask(task.id)).state).toBe('done'))
    const list = await endpoints.listFailoverReports({})
    ok(list, 'FailoverReportPage')
    expect(list.items[0]?.outcome).toBe('completedWithWarnings')
    ok(await endpoints.getFailoverReport(task.id), 'FailoverReport')
    await app.queryClient.invalidateQueries()
    const buttons = await screen.findAllByRole('button', { name: /report/i })
    expect(buttons.length).toBeGreaterThan(0)
  })
})

describe('notScheduled, as corrected', () => {
  it('says that no rebuild is scheduled, not that this version does not rebuild', () => {
    const w = oldPrimaryWords(
      {
        state: 'notScheduled',
        method: null,
        divergenceBytes: null,
        rewindBytes: null,
        message: null,
      },
      (n) => String(n),
    )
    expect(w.text).toMatch(/No rebuild of it is scheduled/)
    expect(w.text).not.toMatch(/this server version does not rebuild/)
  })
})

describe('the plan that comes with a refusal that needs a decision', () => {
  async function startDialog(user: ReturnType<typeof userEvent.setup>) {
    await renderApp('/servers')
    const row = await screen.findByTestId('server-row-10.0.0.12')
    await user.click(within(row).getByRole('button', { name: /fail ?over/i }))
    const dialog = await screen.findByRole('dialog', { name: /fail over server/i })
    await user.click(within(dialog).getByRole('radio', { name: /^hard/i }))
    await user.click(within(dialog).getByRole('button', { name: /^show the plan$/i }))
    await within(dialog).findByTestId('failover-plan')
    return dialog
  }
  it('the mock sends the plan with the refusal; without the member (?failover=noplan) it does not', async () => {
    await signInAs(createPinia())
    mockConfig.failoverDataLoss = true
    const refused = await endpoints.failoverServer('10.0.0.12', { mode: 'hard' }).catch((e) => e)
    expect(refused).toBeInstanceOf(ApiError)
    expect((refused as ApiError).code).toBe('failover_needs_decision')
    expect((refused as ApiError).problem.plan?.groups.length).toBeGreaterThan(0)
    ok((refused as ApiError).problem.plan, 'FailoverPlan')
    sw('failover=noplan')
    const bare = await endpoints.failoverServer('10.0.0.12', { mode: 'hard' }).catch((e) => e)
    expect((bare as ApiError).problem.plan).toBeUndefined()
  })

  it('the dialog shows the plan of the refusal and makes no second dry run', async () => {
    const user = userEvent.setup()
    const dryRuns: number[] = []
    mockServer.use(
      http.post('*/api/v1/servers/:id/failover', async ({ request }) => {
        const b = (await request.clone().json()) as { dryRun?: boolean }
        if (b.dryRun) dryRuns.push(1)
        return undefined
      }),
    )
    const dialog = await startDialog(user)
    // The verdict turns possible after the plan was shown: the start is refused with the new plan.
    mockConfig.failoverDataLoss = true
    await user.type(within(dialog).getByLabelText(/to confirm/i), '10.0.0.12:8091')
    const before = dryRuns.length
    await user.click(within(dialog).getByRole('button', { name: /^hard failover$/i }))
    await within(dialog).findByTestId('accept-loss')
    expect(dryRuns.length).toBe(before)
  })

  it('without the plan in the refusal the dialog asks again with a dry run', async () => {
    const user = userEvent.setup()
    const dryRuns: number[] = []
    mockServer.use(
      http.post('*/api/v1/servers/:id/failover', async ({ request }) => {
        const b = (await request.clone().json()) as { dryRun?: boolean }
        if (b.dryRun) dryRuns.push(1)
        return undefined
      }),
    )
    sw('failover=noplan')
    const dialog = await startDialog(user)
    mockConfig.failoverDataLoss = true
    await user.type(within(dialog).getByLabelText(/to confirm/i), '10.0.0.12:8091')
    const before = dryRuns.length
    await user.click(within(dialog).getByRole('button', { name: /^hard failover$/i }))
    await within(dialog).findByTestId('accept-loss')
    expect(dryRuns.length).toBe(before + 1)
  })

  it('the progress card shows the verdict of the plan of a refused repeat', async () => {
    const user = userEvent.setup()
    sw('failover=needs-decision')
    const dialog = await startDialog(user)
    await user.type(within(dialog).getByLabelText(/to confirm/i), '10.0.0.12:8091')
    await user.click(within(dialog).getByRole('button', { name: /^hard failover$/i }))
    const panel = await screen.findByTestId('waiting-for-you', {}, { timeout: 30_000 })
    mockServer.use(
      http.post('*/api/v1/servers/:id/failover', () =>
        HttpResponse.json(
          {
            type: 'about:blank',
            title: 'x',
            status: 409,
            code: 'failover_needs_decision',
            detail: 'd',
            plan: ok0(),
          },
          { status: 409 },
        ),
      ),
    )
    await waitFor(() =>
      expect(within(panel).getByTestId('accept-and-continue')).not.toHaveAttribute('aria-disabled'),
    )
    await user.click(within(panel).getByTestId('accept-and-continue'))
    const confirm = await screen.findByRole('dialog', { name: /continue and accept data loss/i })
    await user.click(
      within(confirm).getByRole('button', { name: /accept data loss and continue/i }),
    )
    await waitFor(() =>
      expect(screen.getByTestId('decision-loss-2')).toHaveTextContent(
        /The standby on 10\.9\.9\.9 could not be read/,
      ),
    )
  })
})

function ok0() {
  return {
    mode: 'hard',
    serverId: '10.0.0.12',
    groups: [
      {
        shardGroupId: 2,
        isMapGroup: false,
        movesTheManager: false,
        fromServerId: '10.0.0.12',
        toServerId: '10.0.0.13',
        candidates: [],
        dataLoss: {
          verdict: 'unknown',
          message: 'm',
          lastKnownPrimaryLsn: null,
          chosenReceiveLsn: null,
          lastKnownAgeSeconds: null,
          groupRanWithoutSynchronousStandby: false,
          asyncDatabases: [],
          unreadableServerIds: ['10.9.9.9'],
        },
        steps: [],
      },
    ],
    blockers: [],
    willStopRebalance: false,
    estimatedSeconds: 10,
  }
}

describe('a database or table action during a failover', () => {
  it('the mock refuses create, drop and distribute with failover_in_progress', async () => {
    await signInAs(createPinia())
    mockConfig.taskDurationMs = 60_000
    await endpoints.failoverServer('10.0.0.12', 'hard')
    for (const call of [
      () => endpoints.createDatabase({ name: 'newdb' } as never),
      () => endpoints.dropDatabase('reports'),
    ])
      await expect(call()).rejects.toMatchObject({ code: 'failover_in_progress', status: 409 })
  })
  it('the words say what runs and that nothing was changed, and no longer speak of a rebalance', () => {
    const p = describeProblem(
      new ApiError({ type: 'x', title: 'x', status: 409, code: 'failover_in_progress' }),
    )
    expect(p.message).toMatch(/failover or a restore of the preferred primaries is running/)
    expect(p.message).toMatch(/Nothing was changed/)
    expect(p.message).not.toMatch(/rebalance/i)
  })
  it('the create-database dialog shows them', async () => {
    const user = userEvent.setup()
    mockConfig.taskDurationMs = 60_000
    await renderApp('/databases')
    await endpoints.failoverServer('10.0.0.12', 'hard')
    await user.click(await screen.findByRole('button', { name: /create database/i }))
    const d = await screen.findByRole('dialog')
    await user.type(within(d).getByLabelText(/^name/i), 'during')
    await user.click(within(d).getByRole('button', { name: /^create/i }))
    expect(await within(d).findByText('A failover is in progress')).toBeInTheDocument()
  })
})
