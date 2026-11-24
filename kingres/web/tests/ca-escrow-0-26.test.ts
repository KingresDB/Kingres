// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.26.0: the escrow of the CA key. The mock answers bodies that satisfy the schema, the panel under Security shows the status, the
// holders of a copy and the errors, and switching on, off and rotating always go through a confirmation. Only an administrator sees it.
import { beforeEach, describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { createPinia } from 'pinia'
import { endpoints } from '@/api/endpoints'
import { useSessionStore } from '@/stores/session'
import { operations } from '@/api/operations.generated'
import { getState } from '@/mocks/http'
import { applySwitches } from '@/mocks/switches'
import { renderApp, signInAs } from './helpers'
import { ok } from './schema-check'

const sw = (q: string) => applySwitches(new URLSearchParams(q), getState())

async function openPanel(user: ReturnType<typeof userEvent.setup>, as = 'admin') {
  await renderApp('/security', { user: as })
  await user.click(await screen.findByRole('tab', { name: /certificate authority/i }))
  return screen.findByTestId('ca-escrow')
}

describe('the escrow operations and the mock', () => {
  beforeEach(async () => {
    await signInAs(createPinia())
  })

  it('are operations of kingres_admin', () => {
    for (const id of ['getCaEscrow', 'setCaEscrow', 'rotateCaEscrow'] as const)
      expect(operations[id].role).toBe('kingres_admin')
  })

  it('answer schema-valid bodies through on, rotate and off', async () => {
    const off = await endpoints.getCaEscrow()
    ok(off, 'CaEscrowStatus')
    expect(off).toMatchObject({ enabled: false, complete: false, recipients: [] })
    const on = await endpoints.setCaEscrow(true)
    ok(on, 'CaEscrowStatus')
    expect(on).toMatchObject({ enabled: true, complete: true, generation: 1 })
    const rotated = await endpoints.rotateCaEscrow()
    ok(rotated, 'CaEscrowStatus')
    expect(rotated.generation).toBe(2)
    expect((await endpoints.setCaEscrow(false)).enabled).toBe(false)
  })

  it('refuses a rotation while the escrow is off and every change when the leader has no CA key', async () => {
    await expect(endpoints.rotateCaEscrow()).rejects.toMatchObject({ code: 'conflict' })
    sw('escrow=unavailable')
    await expect(endpoints.setCaEscrow(true)).rejects.toMatchObject({
      code: 'certificate_authority_unavailable',
    })
  })
})

describe('the Certificate authority tab of Security', () => {
  it('shows the status off, and switches on only after the confirmation', async () => {
    const user = userEvent.setup()
    const panel = await openPanel(user)
    expect(await within(panel).findByTestId('ca-escrow-state')).toHaveTextContent('Off')
    expect(within(panel).getByTestId('ca-no-recipients')).toHaveTextContent(
      /no server holds a copy/i,
    )
    await user.click(within(panel).getByTestId('ca-escrow-on'))
    const dialog = await screen.findByRole('dialog', { name: /switch the escrow on/i })
    expect(dialog).toHaveTextContent(/can become the CA/i)
    // Nothing has changed yet.
    expect((await endpoints.getCaEscrow()).enabled).toBe(false)
    await user.click(within(dialog).getByRole('button', { name: /^switch escrow on$/i }))
    await waitFor(() =>
      expect(within(panel).getByTestId('ca-escrow-state')).toHaveTextContent('On'),
    )
    const row = await within(panel).findByTestId('ca-recipient-10.0.0.12')
    expect(row).toHaveTextContent(/current/i)
    expect(within(panel).getByTestId('ca-complete')).toHaveTextContent('Yes')
  })

  it('rotates and switches off through confirmations', async () => {
    const user = userEvent.setup()
    sw('escrow=on')
    const panel = await openPanel(user)
    expect(await within(panel).findByTestId('ca-generation')).toHaveTextContent('1')
    await user.click(within(panel).getByTestId('ca-escrow-rotate'))
    await user.click(
      within(await screen.findByRole('dialog', { name: /rotate the escrow keys/i })).getByRole(
        'button',
        { name: /^rotate keys$/i },
      ),
    )
    await waitFor(() => expect(within(panel).getByTestId('ca-generation')).toHaveTextContent('2'))
    await user.click(within(panel).getByTestId('ca-escrow-off'))
    const dialog = await screen.findByRole('dialog', { name: /switch the escrow off/i })
    expect(dialog).toHaveTextContent(/no CA key until an operator restores it/i)
    await user.click(within(dialog).getByRole('button', { name: /^switch escrow off$/i }))
    await waitFor(() =>
      expect(within(panel).getByTestId('ca-escrow-state')).toHaveTextContent('Off'),
    )
  })

  it('lists which standbys hold a copy and which do not', async () => {
    const user = userEvent.setup()
    sw('escrow=standbys')
    const panel = await openPanel(user)
    expect(await within(panel).findByTestId('ca-recipient-10.0.0.12')).toHaveTextContent(/current/i)
    expect(within(panel).getByTestId('ca-recipient-10.0.0.13')).toHaveTextContent(/pending/i)
    expect(within(panel).getByTestId('ca-complete')).toHaveTextContent('No')
    expect(within(panel).getByTestId('ca-message')).toHaveTextContent(/not every server/i)
  })

  it('shows the error text of a refused copy', async () => {
    const user = userEvent.setup()
    sw('escrow=refused')
    const panel = await openPanel(user)
    const row = await within(panel).findByTestId('ca-recipient-10.0.0.12')
    expect(row).toHaveTextContent(/refused/i)
    expect(row).toHaveTextContent(/answered 403/i)
  })

  it('warns that a copy that is still to be deleted counts as knowing the key', async () => {
    const user = userEvent.setup()
    sw('escrow=dropping')
    const panel = await openPanel(user)
    expect(await within(panel).findByTestId('ca-lingering')).toHaveTextContent(
      /10\.0\.0\.12.*knowing the CA key/i,
    )
  })

  it('says what went wrong when the leader holds no CA key, and changes nothing', async () => {
    const user = userEvent.setup()
    sw('escrow=unavailable')
    const panel = await openPanel(user)
    await user.click(await within(panel).findByTestId('ca-escrow-on'))
    const dialog = await screen.findByRole('dialog', { name: /switch the escrow on/i })
    await user.click(within(dialog).getByRole('button', { name: /^switch escrow on$/i }))
    const notice = await within(panel).findByTestId('ca-error')
    expect(notice).toHaveTextContent(/certificate authority not available/i)
    expect(within(panel).getByTestId('ca-escrow-state')).toHaveTextContent('Off')
  })

  it('is not offered to a role below administrator', async () => {
    const { pinia } = await renderApp('/security', { user: 'ops' })
    expect(useSessionStore(pinia).can('getCaEscrow')).toBe(false)
    expect(screen.queryByTestId('ca-escrow')).toBeNull()
    expect(screen.queryByRole('tab', { name: /certificate authority/i })).toBeNull()
  })
})
