// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { getState } from '@/mocks/http'
import { renderApp, settled } from './helpers'

async function openDialog() {
  const user = userEvent.setup()
  await renderApp('/databases')
  await settled()
  await user.click(await screen.findByRole('button', { name: /create database/i }))
  const dialog = await screen.findByRole('dialog', { name: /create database/i })
  return { user, dialog }
}

describe('Create database dialog', () => {
  it('shows the four groups and says what cannot be changed later', async () => {
    const { dialog } = await openDialog()
    for (const name of ['General', 'Sharding', 'Replication', 'Ledger']) {
      expect(within(dialog).getByRole('group', { name })).toBeInTheDocument()
    }
    // Immutable fields are labelled as such.
    expect(within(dialog).getAllByText(/fixed after creation/i).length).toBeGreaterThanOrEqual(3)
    expect(within(dialog).getByText(/cannot be changed later/i)).toBeInTheDocument()
    expect(within(dialog).getByText(/never off again/i)).toBeInTheDocument()
    expect(
      within(dialog).getByText(/cannot be turned off for a table that already has it/i),
    ).toBeInTheDocument()
    // Defaults from the contract.
    expect(within(dialog).getByLabelText(/bucket count/i)).toHaveValue(128)
    expect(within(dialog).getByLabelText(/bucket count/i)).toBeDisabled()
  })

  it('shows the replica count read-only with a link to the cluster settings', async () => {
    const { dialog } = await openDialog()
    await waitFor(() => expect(within(dialog).getByTestId('replica-count')).toHaveTextContent('1'))
    expect(within(dialog).queryByLabelText(/replica count/i)).not.toBeInTheDocument()
    const links = within(dialog).getAllByRole('link', { name: /cluster settings/i })
    expect(links[0]).toHaveAttribute('href', '/settings')
  })

  it('validates against the contract constraints before sending anything', async () => {
    const { user, dialog } = await openDialog()
    await user.type(within(dialog).getByLabelText(/^name/i), 'Bad Name')
    await user.click(within(dialog).getByRole('checkbox', { name: /spread this database/i }))
    const buckets = within(dialog).getByLabelText(/bucket count/i)
    await user.clear(buckets)
    await user.type(buckets, '5000')
    await user.click(within(dialog).getByRole('button', { name: /^create database$/i }))

    expect(
      await within(dialog).findByText(/the name must use lower-case letters/i),
    ).toBeInTheDocument()
    expect(
      within(dialog).getByText(/bucket count must be a whole number between 1 and 4096/i),
    ).toBeInTheDocument()
    expect(within(dialog).getByLabelText(/^name/i)).toHaveAttribute('aria-invalid', 'true')
    expect(getState().databases.map((d) => d.info.name)).toEqual(['shop', 'reports', 'tenants'])
    expect(getState().tasks).toHaveLength(0)
  })

  it('submits, follows the task to the end and shows the new database in the list', async () => {
    const { user, dialog } = await openDialog()
    await user.type(within(dialog).getByLabelText(/^name/i), 'inventory')
    await user.click(within(dialog).getByRole('checkbox', { name: /spread this database/i }))
    await user.selectOptions(within(dialog).getByLabelText(/ledger mode/i), 'appendOnly')
    await user.click(within(dialog).getByRole('button', { name: /^create database$/i }))

    // Progress is announced while the task runs, then it finishes.
    expect(
      await within(dialog).findByRole('progressbar', { name: /task progress/i }),
    ).toBeInTheDocument()
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()

    await user.click(within(dialog).getByRole('button', { name: /^close$/i }))
    const link = await screen.findByRole('link', { name: 'inventory' })
    const row = link.closest('tr')
    expect(row).not.toBeNull()
    expect(within(row as HTMLElement).getByText(/on, 128 buckets/i)).toBeInTheDocument()
    expect(getState().databases.find((d) => d.info.name === 'inventory')?.info.ledger.mode).toBe(
      'appendOnly',
    )
  })

  it('shows a failed task with its error message', async () => {
    const { user, dialog } = await openDialog()
    await user.type(within(dialog).getByLabelText(/^name/i), 'failme')
    await user.click(within(dialog).getByRole('button', { name: /^create database$/i }))
    expect((await within(dialog).findAllByText(/out of disk space/i)).length).toBeGreaterThan(0)
    expect(getState().databases.some((d) => d.info.name === 'failme')).toBe(false)
  })

  it('shows the server answer when the name already exists', async () => {
    const { user, dialog } = await openDialog()
    await user.type(within(dialog).getByLabelText(/^name/i), 'shop')
    await user.click(within(dialog).getByRole('button', { name: /^create database$/i }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(/already exists/i)
  })

  it('closes with Escape and returns focus to the button that opened it', async () => {
    const { user, dialog } = await openDialog()
    expect(dialog).toBeInTheDocument()
    await user.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(screen.getByRole('button', { name: /create database/i })).toHaveFocus()
  })

  it('keeps Tab focus inside the dialog', async () => {
    const { user, dialog } = await openDialog()
    for (let i = 0; i < 40; i++) {
      await user.tab()
      expect(dialog.contains(document.activeElement)).toBe(true)
    }
  })
})
