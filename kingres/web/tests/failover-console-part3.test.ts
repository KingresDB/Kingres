// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The failover console, part 3 (design 8.1, 8.2, 8.6): "Switch over..." on a group card (the planned switchover of one group), and
// "Restore preferred primaries" in the header of the Replication page with its own plan view and refusals. What the server does not
// build yet (`unsupported_operation`) is shown as not available, with nothing changed. What each role sees and does.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http } from 'msw'
import { axe } from 'vitest-axe'
import { mockServer } from '@/mocks/server'
import { endpoints } from '@/api/endpoints'
import { failoverRefusal } from '@/lib/failover'
import { ApiError } from '@/api/problem'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { mockConfig } from '@/mocks/state'
import { ok } from './schema-check'
import { createPinia } from 'pinia'
import { renderApp, signInAs } from './helpers'

const sw = (q: string) => applySwitches(new URLSearchParams(q), getState())
type User = ReturnType<typeof userEvent.setup>

/** A hard failover of 10.0.0.12 that has ended: its group runs away from its home server. */
async function displace(): Promise<void> {
  await signInAs(createPinia())
  mockConfig.taskDurationMs = 60
  const t = await endpoints.failoverServer('10.0.0.12', 'hard')
  await waitFor(async () => expect((await endpoints.getTask(t.id)).state).toBe('done'), {
    timeout: 5000,
  })
}
const restoreButton = () => screen.findByRole('button', { name: /^Restore preferred primaries/ })
async function openRestore(user: User) {
  const b = await restoreButton()
  await waitFor(() => expect(b).not.toHaveAttribute('aria-disabled'))
  await user.click(b)
  return within(await screen.findByRole('dialog', { name: /restore preferred primaries/i }))
}

describe('Switch over... on a group card', () => {
  it('is on every card, and a group without a standby says why it cannot', async () => {
    const app = await renderApp('/replication')
    await screen.findByTestId('group-1')
    for (const id of [0, 1, 2, 3]) {
      const card = screen.queryByTestId(`group-${id}`)
      if (card) expect(within(card).getByTestId(`switch-${id}`)).toBeInTheDocument()
    }
    getState().groups[1]!.standbyServerIds = []
    await app.queryClient.invalidateQueries()
    const b = await screen.findByTestId(`switch-${getState().groups[1]!.id}`)
    await waitFor(() => expect(b).toHaveAttribute('aria-disabled', 'true'))
    expect(b).toHaveTextContent(/no standby to hand over to/i)
  })

  it('opens the switchover of that group: the mode is fixed, the standby is chosen, the plan comes before anything starts', async () => {
    const user = userEvent.setup()
    sw('failover=graceful')
    const bodies: Record<string, unknown>[] = []
    mockServer.use(
      http.post('*/api/v1/servers/:id/failover', async ({ request }) => {
        bodies.push((await request.clone().json()) as Record<string, unknown>)
        return undefined
      }),
    )
    await renderApp('/replication')
    const before = getState().groups.find((x) => x.id === 2)!.primaryServerId
    await user.click(await screen.findByTestId('switch-2'))
    const dialog = within(await screen.findByRole('dialog', { name: /^Switch over Group 2/ }))
    expect(dialog.getByTestId('switchover-intro')).toHaveTextContent(/without losing a commit/i)
    expect(dialog.queryByRole('radio')).not.toBeInTheDocument()
    expect(dialog.queryByRole('checkbox')).not.toBeInTheDocument()
    await user.click(dialog.getByRole('button', { name: /^show the plan$/i }))
    expect(await dialog.findByTestId('failover-plan')).toBeInTheDocument()
    expect(bodies[0]).toMatchObject({ mode: 'graceful', shardGroupIds: [2], dryRun: true })
    // The switchover needs no typed address and no decision about lost data.
    expect(dialog.queryByLabelText(/to confirm/i)).not.toBeInTheDocument()
    expect(dialog.queryByTestId('accept-loss')).not.toBeInTheDocument()
    await user.click(dialog.getByRole('button', { name: /^switch over$/i }))
    await dialog.findByRole('progressbar')
    expect(bodies[1]).toMatchObject({ mode: 'graceful', shardGroupIds: [2] })
    expect(bodies[1]).not.toHaveProperty('dryRun')
    await waitFor(
      () => expect(getState().groups.find((x) => x.id === 2)!.primaryServerId).not.toBe(before),
      {
        timeout: 8000,
      },
    )
  })

  it('a server that does not build the switchover yet: not available, nothing changed', async () => {
    const user = userEvent.setup()
    await renderApp('/replication')
    const before = getState().groups.find((x) => x.id === 2)!.primaryServerId
    await user.click(await screen.findByTestId('switch-2'))
    const dialog = within(await screen.findByRole('dialog', { name: /^Switch over Group 2/ }))
    await user.click(dialog.getByRole('button', { name: /^show the plan$/i }))
    const notice = await dialog.findByTestId('failover-refusal')
    expect(notice).toHaveTextContent('Not available on this server version yet')
    expect(notice).toHaveTextContent(
      /planned switchover of a group is not part of this server version yet/i,
    )
    expect(notice).not.toHaveTextContent(/hard mode/i)
    expect(notice).toHaveTextContent(/Nothing was changed/)
    expect(getState().groups.find((x) => x.id === 2)!.primaryServerId).toBe(before)
    expect(getState().tasks.some((t) => t.task.kind === 'server.failover')).toBe(false)
  })

  it('only kingres_ops and kingres_admin may start it: read-only sees the button disabled with the reason', async () => {
    for (const [user, enabled] of [
      ['viewer', false],
      ['ops', true],
      ['admin', true],
    ] as const) {
      const app = await renderApp('/replication', { user })
      const b = await screen.findByTestId('switch-2')
      await waitFor(() => expect(b.getAttribute('aria-disabled') === 'true').toBe(!enabled))
      if (!enabled) expect(b).toHaveTextContent(/kingres_ops/)
      app.unmount()
    }
  })
})

describe('Restore preferred primaries', () => {
  it('the header says how many groups are away; with none the button is blocked with the reason', async () => {
    await renderApp('/replication')
    expect(await screen.findByTestId('displaced-count')).toHaveTextContent(
      'Every group is on its preferred server.',
    )
    const b = await restoreButton()
    expect(b).toHaveAttribute('aria-disabled', 'true')
    expect(b).toHaveTextContent(/already on its preferred server/i)
  })

  it('shows the groups that are away, the plan, and starts the restore for all of them', async () => {
    const user = userEvent.setup()
    await displace()
    const bodies: Record<string, unknown>[] = []
    mockServer.use(
      http.post('*/api/v1/replication/restore-preferred-primaries', async ({ request }) => {
        bodies.push((await request.clone().json()) as Record<string, unknown>)
        return undefined
      }),
    )
    await renderApp('/replication')
    await waitFor(() =>
      expect(screen.getByTestId('displaced-count')).toHaveTextContent(
        /[0-9]+ groups? (is|are) away from the preferred server/,
      ),
    )
    const d = await openRestore(user)
    const away = getState().groups.filter((g) => g.primaryServerId !== g.preferredPrimaryServerId)
    expect(away.length).toBeGreaterThan(0)
    for (const g of away) expect(d.getByTestId(`restore-choice-${g.id}`)).toBeInTheDocument()
    expect(d.getByTestId('restore-groups')).toHaveTextContent(/back to server/)
    await user.click(d.getByRole('button', { name: /^show the plan$/i }))
    expect(await d.findByTestId('failover-plan')).toBeInTheDocument()
    expect(bodies[0]).toMatchObject({ dryRun: true })
    expect(bodies[0]).not.toHaveProperty('shardGroupIds')
    expect(d.queryByTestId('accept-loss')).not.toBeInTheDocument()
    await user.click(d.getByRole('button', { name: /^restore primaries$/i }))
    await waitFor(() => expect(bodies).toHaveLength(2))
    expect(bodies[1]).toEqual({})
    await waitFor(
      () =>
        expect(
          getState().groups.every((g) => g.primaryServerId === g.preferredPrimaryServerId),
        ).toBe(true),
      { timeout: 8000 },
    )
  })

  it('a group that is left out is not sent: the request names the others', async () => {
    const user = userEvent.setup()
    // Two groups run away from their home server (the state is set, not made by a failover).
    for (const g of getState().groups.filter((x) => [1, 2].includes(x.id))) {
      const other = g.standbyServerIds[0]
      if (other) g.primaryServerId = other
    }
    const away = getState().groups.filter((g) => g.primaryServerId !== g.preferredPrimaryServerId)
    expect(away.length).toBe(2)
    const bodies: Record<string, unknown>[] = []
    mockServer.use(
      http.post('*/api/v1/replication/restore-preferred-primaries', async ({ request }) => {
        bodies.push((await request.clone().json()) as Record<string, unknown>)
        return undefined
      }),
    )
    await renderApp('/replication')
    const d = await openRestore(user)
    await user.click(within(d.getByTestId(`restore-choice-${away[0]!.id}`)).getByRole('checkbox'))
    await user.click(d.getByRole('button', { name: /^show the plan$/i }))
    await d.findByTestId('failover-plan')
    expect((bodies[0]?.shardGroupIds as number[]).includes(away[0]!.id)).toBe(false)
    expect((bodies[0]?.shardGroupIds as number[]).length).toBe(away.length - 1)
  })

  it('a server that does not build the restore yet: not available, nothing changed', async () => {
    const user = userEvent.setup()
    await displace()
    mockConfig.unsupported.add('restorePreferredPrimaries')
    await renderApp('/replication')
    const d = await openRestore(user)
    await user.click(d.getByRole('button', { name: /^show the plan$/i }))
    const notice = await d.findByTestId('failover-refusal')
    expect(notice).toHaveTextContent('Not available on this server version yet')
    expect(notice).toHaveTextContent(
      /Restoring the preferred primaries .* is not part of this server version yet/,
    )
    expect(notice).toHaveTextContent(/Nothing was changed/)
    expect(
      getState().tasks.some((t) => t.task.kind === 'replication.restorePreferredPrimaries'),
    ).toBe(false)
  })

  it('a blocker of the plan is listed and the start stays disabled; a lost majority is a refusal in words', async () => {
    const user = userEvent.setup()
    await displace()
    sw('failover=blocked:lease')
    await renderApp('/replication')
    let d = await openRestore(user)
    await user.click(d.getByRole('button', { name: /^show the plan$/i }))
    await d.findByTestId('failover-plan')
    expect(d.getByTestId('plan-stuck')).toBeInTheDocument()
    expect(d.getByRole('button', { name: /^restore primaries$/i })).toBeDisabled()
    await user.click(d.getByRole('button', { name: /^cancel$/i }))
    mockConfig.failoverBlock = 'quorum'
    d = await openRestore(user)
    await user.click(d.getByRole('button', { name: /^show the plan$/i }))
    expect(
      (await d.findAllByText(/hear a majority|majority of the servers/i)).length,
    ).toBeGreaterThan(0)
  })

  it('a failover that runs refuses the restore with its own words (failover_in_progress)', async () => {
    const user = userEvent.setup()
    await displace()
    await renderApp('/replication')
    const d = await openRestore(user)
    await user.click(d.getByRole('button', { name: /^show the plan$/i }))
    await d.findByTestId('failover-plan')
    mockConfig.taskDurationMs = 60_000
    sw('failover=auto:10.0.0.13')
    await user.click(d.getByRole('button', { name: /^restore primaries$/i }))
    const notice = await d.findByTestId('failover-refusal')
    expect(notice).toHaveTextContent('A failover is already running')
  })

  it('only kingres_ops and kingres_admin may restore', async () => {
    await displace()
    const app = await renderApp('/replication', { user: 'viewer' })
    const b = await restoreButton()
    expect(b).toHaveAttribute('aria-disabled', 'true')
    expect(b).toHaveTextContent(/kingres_ops/)
    app.unmount()
    const ops = await renderApp('/replication', { user: 'ops' })
    await waitFor(async () => expect(await restoreButton()).not.toHaveAttribute('aria-disabled'))
    ops.unmount()
  })

  it('the mock satisfies the contract: the plan and the task', async () => {
    await displace()
    await renderApp('/replication')
    ok(await endpoints.previewRestore({}), 'FailoverPlan')
    expect(await endpoints.previewRestore({ shardGroupIds: [999] })).toMatchObject({ groups: [] })
  })
})

describe('the words of the refusals of a restore and a switchover', () => {
  const err = (code: string) =>
    new ApiError({ type: 'x', title: 'x', status: 409, code: code as never } as never)
  it('unsupported_operation says what is not available for each dialog', () => {
    const restore = failoverRefusal(err('unsupported_operation'), {
      mode: 'restore',
      includesMapGroup: false,
    })
    expect(restore?.message).toMatch(/Restoring the preferred primaries/)
    const sw1 = failoverRefusal(err('unsupported_operation'), {
      mode: 'graceful',
      includesMapGroup: false,
      switchover: true,
    })
    expect(sw1?.message).toMatch(/switchover of a group is not part/)
    expect(sw1?.message).not.toMatch(/hard mode/)
    const sw2 = failoverRefusal(err('unsupported_operation'), {
      mode: 'graceful',
      includesMapGroup: false,
    })
    expect(sw2?.message).toMatch(/hard mode/)
  })
})

describe('accessibility', () => {
  it('the switchover dialog and the restore dialog, in the choice and in the plan', async () => {
    const user = userEvent.setup()
    sw('failover=graceful')
    const app = await renderApp('/replication')
    const open = await screen.findByTestId('switch-2')
    await waitFor(() => expect(open).not.toHaveAttribute('aria-disabled'))
    await user.click(open)
    let root = await screen.findByRole('dialog')
    expect(await axe(root)).toHaveNoViolations()
    await user.click(within(root).getByRole('button', { name: /^show the plan$/i }))
    await within(root).findByTestId('failover-plan')
    expect(await axe(root)).toHaveNoViolations()
    await user.click(within(root).getByRole('button', { name: /^cancel$/i }))
    await displace()
    await app.queryClient.invalidateQueries()
    const d = await openRestore(user)
    root = await screen.findByRole('dialog')
    expect(await axe(root)).toHaveNoViolations()
    await user.click(d.getByRole('button', { name: /^show the plan$/i }))
    await d.findByTestId('failover-plan')
    expect(await axe(root)).toHaveNoViolations()
  })
})
