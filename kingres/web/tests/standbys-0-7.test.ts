// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.7.0, standbys: the replication view lists the standbys kingresd manages (state, lag, synchronous or not,
// `instanceId` 1048576 plus their own id), says whether a commit needs a standby, and an administrator changes the
// replica count through the settings operation, which starts the task `standby.reconcile`; its step
// `standby.register_in_catalog` is skipped on purpose and must not look like a failure.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { mockServer } from '@/mocks/server'
import { getState } from '@/mocks/http'
import {
  CATALOG_SKIPPED_DETAIL,
  mockConfig,
  silenceServer,
  stallStandby,
  startStandbyReconcile,
} from '@/mocks/state'
import { HOSTILE } from '@/mocks/hostile'
import { commitRule, managedStandbyId, skippedReason, taskDiscovery } from '@/lib/standby'
import { TASK_KIND_LABEL } from '@/lib/status'
import { renderApp } from './helpers'

taskDiscovery.intervalMs = 20
taskDiscovery.attempts = 6

const group = async (id: number) => within(await screen.findByTestId(`group-${id}`))

async function changeCount(user: ReturnType<typeof userEvent.setup>, value: string) {
  await user.click(await screen.findByRole('button', { name: /change replica count/i }))
  const dialog = await screen.findByRole('dialog', { name: /change the replica count/i })
  const input = within(dialog).getByLabelText(/standbys per shard group/i)
  await user.clear(input)
  await user.type(input, value)
  return dialog
}

describe('what the Replication page shows about the standbys', () => {
  it('says for every group whether a commit waits for a standby, with the standby and its lag', async () => {
    await renderApp('/replication')
    const g1 = await group(1)
    expect(g1.getByTestId('commit-rule')).toHaveAttribute('data-state', 'yes')
    expect(g1.getByTestId('commit-rule')).toHaveTextContent(
      /waits until the standby that streams has flushed it/i,
    )
    // Primary and standby, with state, sync state and lag.
    const rows = g1.getAllByRole('row')
    expect(rows.length).toBe(3)
    expect(within(rows[1]!).getByText('primary')).toBeInTheDocument()
    expect(within(rows[2]!).getByText('standby')).toBeInTheDocument()
    expect(within(rows[2]!).getByText('Quorum')).toBeInTheDocument()
    expect(within(rows[2]!).getByText(/a commit can wait for it/i)).toBeInTheDocument()
    expect(within(rows[2]!).getByText('Running')).toBeInTheDocument()
    expect(within(rows[2]!).getByText(/ms|s$/)).toBeInTheDocument()
    expect(screen.getByTestId('replica-count')).toHaveTextContent('1')
  })

  it('says a commit needs no standby in a group that has none', async () => {
    getState().settings.defaultReplicaCount = 0
    getState().groups.find((x) => x.id === 2)!.standbyServerIds = []
    await renderApp('/replication')
    const g2 = await group(2)
    expect(g2.getByTestId('commit-rule')).toHaveAttribute('data-state', 'no')
    expect(g2.getByTestId('commit-rule')).toHaveTextContent(
      /no standby, so a commit is acknowledged by the primary alone/i,
    )
    expect(g2.getByText('No standby')).toBeInTheDocument()
  })

  it('a standby that stopped streaming is still waited for during the grace period', async () => {
    stallStandby(getState(), 2, '10.0.0.13')
    await renderApp('/replication')
    const g2 = await group(2)
    expect(g2.getByTestId('commit-rule')).toHaveAttribute('data-state', 'unknown')
    expect(g2.getByTestId('commit-rule')).toHaveTextContent(/grace period.*still waits/is)
    expect(g2.getByText('Degraded')).toBeInTheDocument()
    expect(g2.getByText('Async')).toBeInTheDocument()
    expect(g2.getByText(/a commit does not wait for it/i)).toBeInTheDocument()
  })

  it('after the grace period the primary no longer waits, the alert is raised and the group says so', async () => {
    stallStandby(getState(), 2, '10.0.0.13', Date.now() - (mockConfig.syncGraceMs + 1000))
    await renderApp('/replication')
    const g2 = await group(2)
    await waitFor(() => expect(g2.getByTestId('commit-rule')).toHaveAttribute('data-state', 'no'))
    expect(g2.getByTestId('commit-rule')).toHaveTextContent(/no, not any more/i)
    expect(g2.getByTestId('commit-rule')).toHaveTextContent(/acknowledged by one server only/i)
    const alert = getState().alerts.find((a) => a.kind === 'groupWithoutSynchronousStandby')
    expect(alert?.severity).toBe('critical')
    expect(alert?.subject?.shardGroupId).toBe(2)
  })

  it('the alert clears and the rule returns when the standby streams again', async () => {
    const s = getState()
    stallStandby(s, 2, '10.0.0.13', Date.now() - (mockConfig.syncGraceMs + 1000))
    await renderApp('/replication')
    await waitFor(async () =>
      expect((await group(2)).getByTestId('commit-rule')).toHaveAttribute('data-state', 'no'),
    )
    s.groups.find((x) => x.id === 2)!.stalled = {}
    await waitFor(
      async () =>
        expect((await group(2)).getByTestId('commit-rule')).toHaveAttribute('data-state', 'yes'),
      { timeout: 30_000 },
    )
    expect(s.alerts.find((a) => a.kind === 'groupWithoutSynchronousStandby')?.state).toBe(
      'resolved',
    )
  })

  it('a server that stops answering also stops its standbys from streaming', async () => {
    silenceServer(getState(), '10.0.0.13', Date.now() - 1000)
    await renderApp('/replication')
    const g2 = await group(2)
    expect(g2.getByText('Degraded')).toBeInTheDocument()
    expect(g2.getByTestId('commit-rule')).toHaveAttribute('data-state', 'unknown')
  })

  it('commitRule reads the members, never a message', () => {
    const member = (over: object) =>
      ({
        instanceId: 1,
        serverId: 's',
        role: 'standby',
        state: 'running',
        port: 1,
        timeline: 1,
        syncState: 'quorum',
        lagBytes: 0,
        lagSeconds: 0,
        isOnPreferredServer: true,
        ...over,
      }) as never
    const base = {
      id: 4,
      isMapGroup: false,
      preferredPrimaryServerId: 's',
      health: 'healthy',
      slots: [],
      primary: null,
    }
    const two = commitRule(
      { ...base, standbys: [member({}), member({ instanceId: 2 })] } as never,
      null,
    )
    expect(two.state).toBe('yes')
    expect(two.text).toMatch(/any one of the 2 standbys/)
    const starting = commitRule(
      {
        ...base,
        standbys: [member({ state: 'starting', syncState: 'async', lagBytes: null })],
      } as never,
      null,
    )
    expect(starting.label).toBe('Not yet')
    expect(starting.text).toMatch(/does not count until it streams/)
  })

  it('tells a managed standby by its instance id', () => {
    expect(managedStandbyId({ role: 'standby', instanceId: 1048577 })).toBe(1)
    expect(managedStandbyId({ role: 'standby', instanceId: 223 })).toBeNull()
    expect(managedStandbyId({ role: 'primary', instanceId: 2000000 })).toBeNull()
  })
})

describe('changing the replica count', () => {
  it('an administrator raises it: the dialog follows the task, the new standbys appear, one step is skipped on purpose', async () => {
    const user = userEvent.setup()
    await renderApp('/replication')
    const dialog = await changeCount(user, '2')
    await user.click(within(dialog).getByRole('button', { name: /^change replica count$/i }))
    // The task that the settings change started, with its progress.
    await within(dialog).findByLabelText(/task \d+ progress/i)
    await within(dialog).findByTestId('replica-done', {}, { timeout: 30_000 })
    expect(getState().settings.defaultReplicaCount).toBe(2)
    // It is not a rebalance, and a standby was created for every group.
    expect(getState().rebalance.reasons).not.toContain('replicaCountChanged')
    for (const g of getState().groups) expect(g.standbyServerIds).toHaveLength(2)
    // The catalog step is shown as skipped with its reason, not as a failure.
    const skipped = within(dialog).getByTestId('step-skipped')
    expect(skipped).toHaveTextContent(/skipped on purpose, not a failure/i)
    expect(skipped).toHaveTextContent(CATALOG_SKIPPED_DETAIL.replace('skipped: ', ''))
    expect(within(dialog).getByTestId('task-skip-note')).toHaveTextContent(/not a failure/i)
    const step = within(dialog).getByText('standby.register_in_catalog').closest('li')
    expect(step).toHaveAttribute('data-state', 'skipped')
    expect(within(dialog).queryByText(/failed/i)).toBeNull()
    await user.click(within(dialog).getAllByRole('button', { name: /close/i }).at(-1)!)
    // The new standbys are in the list with the instance id of a managed standby, and they stream.
    const g1 = await group(1)
    await waitFor(() => expect(g1.getAllByTestId('managed-standby').length).toBe(1), {
      timeout: 30_000,
    })
    expect(g1.getByTestId('managed-standby')).toHaveTextContent(/standby \d+, managed by kingresd/)
    expect(g1.getByText(/#10485\d\d/)).toBeInTheDocument()
    expect(screen.getByTestId('replica-count')).toHaveTextContent('2')
  }, 90_000)

  it('a standby that is being created is starting, does not stream and holds up no commit', async () => {
    const s = getState()
    s.settings.defaultReplicaCount = 2
    s.groups.find((x) => x.id === 1)!.standbyServerIds = ['10.0.0.12', '10.0.0.13']
    s.groups.find((x) => x.id === 1)!.creating = ['10.0.0.13']
    s.groups.find((x) => x.id === 1)!.managed = ['10.0.0.13']
    s.groups.find((x) => x.id === 1)!.standbyIds = { '10.0.0.13': 9 }
    await renderApp('/replication')
    const g1 = await group(1)
    expect(g1.getByText('Starting')).toBeInTheDocument()
    expect(g1.getByText('not streaming')).toBeInTheDocument()
    expect(g1.getByText('#1048585', { exact: false })).toBeInTheDocument()
    // The standby that streams still counts.
    expect(g1.getByTestId('commit-rule')).toHaveAttribute('data-state', 'yes')
  })

  it('lowering it warns first and removes the standbys with the step "Remove the standby"', async () => {
    const user = userEvent.setup()
    await renderApp('/replication')
    const dialog = await changeCount(user, '0')
    expect(within(dialog).getByTestId('replica-lowering')).toHaveTextContent(
      /removed with their replication slots/i,
    )
    await user.click(within(dialog).getByRole('button', { name: /^change replica count$/i }))
    await within(dialog).findByTestId('replica-done', {}, { timeout: 30_000 })
    expect(
      within(dialog).getAllByText(/remove the standby of shard group/i).length,
    ).toBeGreaterThan(0)
    for (const g of getState().groups) expect(g.standbyServerIds).toHaveLength(0)
    expect(getState().audit.filter((e) => e.action === 'standbyRemoved').length).toBe(4)
  }, 90_000)

  it('saving the value it already has changes nothing and says no task was started', async () => {
    const user = userEvent.setup()
    await renderApp('/replication')
    const dialog = await changeCount(user, '1')
    await user.click(within(dialog).getByRole('button', { name: /^change replica count$/i }))
    expect(await within(dialog).findByTestId('replica-none')).toHaveTextContent(
      /no standby task was started/i,
    )
    expect(getState().tasks.some((t) => t.task.kind === 'standby.reconcile')).toBe(false)
  })

  it('a value above 3 is refused before it is sent', async () => {
    const user = userEvent.setup()
    await renderApp('/replication')
    const dialog = await changeCount(user, '4')
    await user.click(within(dialog).getByRole('button', { name: /^change replica count$/i }))
    expect(await within(dialog).findByText(/between 0 and 3/i)).toBeInTheDocument()
    expect(getState().settings.defaultReplicaCount).toBe(1)
  })

  it('more standbys than the servers allow: as many as fit, and the group says it is under-replicated', async () => {
    const user = userEvent.setup()
    await renderApp('/replication')
    const dialog = await changeCount(user, '3')
    expect(within(dialog).getByTestId('replica-raising')).toHaveTextContent(
      /only 2 can be created/i,
    )
    await user.click(within(dialog).getByRole('button', { name: /^change replica count$/i }))
    await within(dialog).findByTestId('replica-done', {}, { timeout: 30_000 })
    for (const g of getState().groups) expect(g.standbyServerIds).toHaveLength(2)
    await user.click(within(dialog).getAllByRole('button', { name: /close/i }).at(-1)!)
    await waitFor(() =>
      expect(
        getState().alerts.some((a) => a.kind === 'groupUnderReplicated' && a.state !== 'resolved'),
      ).toBe(true),
    )
  }, 90_000)

  it('the error of the settings operation is shown in the dialog', async () => {
    mockServer.use(
      http.patch('*/api/v1/settings/cluster', () =>
        HttpResponse.json(
          {
            type: 'about:blank',
            title: 'No',
            status: 409,
            code: 'conflict',
            detail: 'Settings are locked.',
          },
          { status: 409 },
        ),
      ),
    )
    const user = userEvent.setup()
    await renderApp('/replication')
    const dialog = await changeCount(user, '2')
    await user.click(within(dialog).getByRole('button', { name: /^change replica count$/i }))
    expect(await within(dialog).findByText(/settings are locked/i)).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: /^change replica count$/i })).toBeEnabled()
  })

  it('an operator and a read-only account see the count but cannot change it', async () => {
    for (const user of ['ops', 'viewer']) {
      const { unmount } = await renderApp('/replication', { user })
      const button = await screen.findByRole('button', { name: /change replica count/i })
      expect(button).toHaveAttribute('aria-disabled', 'true')
      await waitFor(() => expect(screen.getByTestId('replica-count')).toHaveTextContent('1'))
      unmount()
    }
  })

  it('the settings page no longer says the change needs a rebalance', async () => {
    await renderApp('/settings')
    expect(await screen.findByText(/it is not a rebalance/i)).toBeInTheDocument()
    expect(screen.queryByText(/needs a rebalance/i)).toBeNull()
  })

  it('shows the last standby task on the page, with the skipped step', async () => {
    const s = getState()
    s.settings.defaultReplicaCount = 2
    startStandbyReconcile(s, 'admin')
    await renderApp('/replication')
    const box = await screen.findByTestId('standby-task')
    await waitFor(() => expect(within(box).getByText(/last standby task/i)).toBeInTheDocument(), {
      timeout: 30_000,
    })
    expect(within(box).getByTestId('step-skipped')).toBeInTheDocument()
  })
})

describe('the task standby.reconcile', () => {
  it('has words', () => {
    expect(TASK_KIND_LABEL['standby.reconcile']).toBe('Add or remove standbys')
  })

  it('shows a step in the state skipped (the contract enum) as skipped, with its detail as the reason', async () => {
    const now = new Date().toISOString()
    const task = {
      id: 901,
      kind: 'standby.reconcile',
      state: 'done',
      progress: {
        percent: 100,
        currentStep: null,
        steps: [
          { name: 'Send the list of standbys to every server', state: 'done' },
          {
            name: 'standby.register_in_catalog',
            state: 'skipped',
            detail: 'skipped: needs kingres_shard 0.5',
          },
        ],
      },
      error: null,
      detail: null,
      subject: { type: 'replication', id: 'standbys' },
      cancellable: false,
      createdAt: now,
      updatedAt: now,
      createdBy: 'system',
    }
    mockServer.use(
      http.get('*/api/v1/tasks', () => HttpResponse.json({ items: [task], nextCursor: null })),
    )
    await renderApp('/tasks')
    const row = (await screen.findAllByText('Add or remove standbys'))[0]
    expect(row).toBeInTheDocument()
    await renderApp('/replication')
    const box = await screen.findByTestId('standby-task')
    expect(within(box).getByTestId('step-skipped')).toHaveTextContent(/needs kingres_shard 0.5/)
    expect(within(box).getByText('Done')).toBeInTheDocument()
  })

  it('a failed task is not softened: its failed step is a failure', async () => {
    const now = new Date().toISOString()
    mockServer.use(
      http.get('*/api/v1/tasks', () =>
        HttpResponse.json({
          items: [
            {
              id: 902,
              kind: 'standby.reconcile',
              state: 'failed',
              progress: {
                percent: 40,
                currentStep: null,
                steps: [
                  {
                    name: 'Create the standby of shard group 1 on server 10.0.0.13',
                    state: 'failed',
                  },
                  { name: 'standby.register_in_catalog', state: 'skipped' },
                ],
              },
              error: {
                code: 'x',
                message: 'The base backup failed.',
                step: 'Create',
                retryable: true,
              },
              detail: null,
              subject: { type: 'replication', id: 'standbys' },
              cancellable: false,
              createdAt: now,
              updatedAt: now,
              createdBy: 'system',
            },
          ],
          nextCursor: null,
        }),
      ),
    )
    await renderApp('/replication')
    const box = await screen.findByTestId('standby-task')
    expect(within(box).getByText('Failed')).toBeInTheDocument()
    expect(within(box).getByText(/the base backup failed/i)).toBeInTheDocument()
    // The step that was not run because an earlier one failed is not presented as skipped on purpose.
    expect(within(box).queryByTestId('step-skipped')).toBeNull()
    expect(within(box).queryByTestId('task-skip-note')).toBeNull()
  })

  it('shows a hostile reason and hostile server ids as text', async () => {
    const s = getState()
    s.settings.defaultReplicaCount = 2
    const task = startStandbyReconcile(s, HOSTILE.reversed)!
    s.tasks.find((t) => t.task.id === task.id)!.task.detail = `skipped: ${HOSTILE.markup}`
    s.groups.find((x) => x.id === 1)!.standbyServerIds = ['10.0.0.12']
    s.servers[2]!.address = HOSTILE.breakout
    await renderApp('/replication')
    await screen.findByTestId('standby-task')
    expect(document.querySelector('img[src="x"]')).toBeNull()
    expect(document.querySelector('svg[onload]')).toBeNull()
  })

  it('skippedReason reads only the text the server records', () => {
    expect(skippedReason('skipped: needs kingres_shard 0.5')).toBe('needs kingres_shard 0.5')
    expect(skippedReason('Skipped:')).toBe('no reason given')
    expect(skippedReason('waiting for server 3')).toBeNull()
    expect(skippedReason(null)).toBeNull()
  })
})
