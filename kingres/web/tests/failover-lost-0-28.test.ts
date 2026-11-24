// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.28.0: `lostServerId` of abandonMapGroupBootstrap. The mock keeps the rules (the server must be the candidate and must not be heard), and the
// abandon dialog of the progress panel offers the declaration, with a strong warning, and shows the 409 refusals with their text.
import { beforeEach, describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { createPinia } from 'pinia'
import { endpoints } from '@/api/endpoints'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { mockConfig, silenceServer } from '@/mocks/state'
import { renderApp, signInAs } from './helpers'
import { ok } from './schema-check'

type User = ReturnType<typeof userEvent.setup>

/** Starts a hard failover of the answering server (it holds the map group) and waits until the manager is moving. */
async function movingManager(user: User): Promise<void> {
  applySwitches(new URLSearchParams('failover=map'), getState())
  mockConfig.taskDurationMs = 60_000
  await renderApp('/servers')
  const row = await screen.findByTestId('server-row-10.0.0.11')
  await user.click(within(row).getByRole('button', { name: /fail ?over/i }))
  const dialog = await screen.findByRole('dialog', { name: /fail over server/i })
  await user.click(within(dialog).getByRole('radio', { name: /^hard/i }))
  await user.click(within(dialog).getByRole('button', { name: /^show the plan$/i }))
  await within(dialog).findByTestId('plan-group-0')
  await user.type(within(dialog).getByLabelText(/to confirm/i), '10.0.0.11:8091')
  await user.click(within(dialog).getByRole('button', { name: /^hard failover$/i }))
  await within(dialog).findByRole('progressbar')
  await user.click(within(dialog).getByRole('button', { name: /hide/i }))
  await screen.findByTestId('failover-progress')
  const rec = getState().tasks.find((t) => t.task.kind === 'server.failover')
  if (rec) rec.startedAt = Date.now() - (4 / 22) * rec.durationMs
  await screen.findByTestId('manager-moving', {}, { timeout: 30_000 })
}

describe('abandonMapGroupBootstrap with lostServerId (mock)', () => {
  beforeEach(async () => {
    await signInAs(createPinia())
  })

  it('refuses a body without a bootstrap in progress, with a text', async () => {
    await expect(endpoints.abandonMapGroupBootstrap('a test', '10.0.0.12')).rejects.toMatchObject({
      code: 'conflict',
    })
    ok(await endpoints.abandonMapGroupBootstrap('a test'), 'AbandonMapGroupBootstrapResult')
  })
})

describe('the abandon dialog of the progress panel', () => {
  it('offers the candidate, warns strongly, and shows the refusal when the server is still heard', async () => {
    const user = userEvent.setup()
    await movingManager(user)
    await user.click(screen.getByTestId('abandon-bootstrap'))
    const dialog = await screen.findByRole('dialog', { name: /give up the move of the manager/i })
    // Off by default, and no warning until the box is checked.
    const box = within(dialog).getByRole('checkbox', {
      name: /declare this server lost: 10\.0\.0\.12/i,
    })
    expect(box).not.toBeChecked()
    expect(within(dialog).queryByTestId('declare-lost-warning')).toBeNull()
    await user.click(box)
    const warning = within(dialog).getByTestId('declare-lost-warning')
    expect(warning).toHaveTextContent(/only for a server that is really gone/i)
    expect(warning).toHaveTextContent(/accepted as lost/i)
    await user.click(within(dialog).getByRole('button', { name: /^give up the move$/i }))
    await waitFor(() =>
      expect(screen.queryByRole('dialog', { name: /give up the move/i })).toBeNull(),
    )
    // The 409 is shown with the text the server gave.
    const notice = await screen.findByText(/still heard by the cluster/i)
    expect(notice.closest('[data-code]')).toHaveAttribute('data-code', 'conflict')
  })

  it('sends the declaration for a server that is gone and says the bootstrap was given up', async () => {
    const user = userEvent.setup()
    await movingManager(user)
    silenceServer(getState(), '10.0.0.12', Date.now() - mockConfig.failureMs - 1000)
    await user.click(screen.getByTestId('abandon-bootstrap'))
    const dialog = await screen.findByRole('dialog', { name: /give up the move of the manager/i })
    await user.click(within(dialog).getByTestId('declare-lost-check'))
    await user.click(within(dialog).getByRole('button', { name: /^give up the move$/i }))
    expect(await screen.findByTestId('abandon-note')).toHaveTextContent(
      /the bootstrap was given up/i,
    )
  })

  it('sends no declaration when the box is not checked: the old 409 stays', async () => {
    const user = userEvent.setup()
    await movingManager(user)
    await user.click(screen.getByTestId('abandon-bootstrap'))
    const dialog = await screen.findByRole('dialog', { name: /give up the move of the manager/i })
    await user.click(within(dialog).getByRole('button', { name: /^give up the move$/i }))
    expect(await screen.findByText(/promoted already/i)).toBeInTheDocument()
  })
})
