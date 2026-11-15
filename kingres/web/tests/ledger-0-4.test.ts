// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Ledger behaviour of contract 0.4.0: inclusion proofs and digest documents saved byte for byte for
// `kingres-verify`, the refusals of createInclusionProof, and the verification records.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { getState } from '@/mocks/http'
import { mockConfig } from '@/mocks/state'
import { renderApp } from './helpers'

/** Every body the panel read from these paths, as text, and every file it asked the browser to save. */
const bodies: { path: string; text: string }[] = []
const saved: { name: string; blob: Blob }[] = []

async function savedText(index = saved.length - 1): Promise<string> {
  const blob = saved[index]?.blob
  if (!blob) throw new Error('nothing was saved')
  return await new Promise((resolve) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result))
    reader.readAsText(blob)
  })
}

beforeEach(() => {
  const realFetch = globalThis.fetch // the one the mock service worker installed
  bodies.length = 0
  saved.length = 0
  vi.spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
    const res = await realFetch(input, init)
    const url = new URL(res.url || String(input instanceof Request ? input.url : input))
    if (/\/ledger\/(digests|inclusion-proofs)/.test(url.pathname))
      bodies.push({ path: url.pathname, text: await res.clone().text() })
    return res
  })
  URL.createObjectURL = (blob: Blob) => {
    saved.push({ name: '', blob })
    return `blob:test/${saved.length}`
  }
  URL.revokeObjectURL = () => undefined
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (
    this: HTMLAnchorElement,
  ) {
    const last = saved[saved.length - 1]
    if (last) last.name = this.download
  })
})
afterEach(() => vi.restoreAllMocks())

async function openLedger() {
  const user = userEvent.setup()
  await renderApp('/ledger')
  await screen.findByText(/status of shop/i)
  return user
}

async function askProof(user: ReturnType<typeof userEvent.setup>, key: string) {
  const field = screen.getByLabelText('Primary key (JSON)')
  await user.clear(field)
  await user.click(field)
  await user.paste(key)
  await user.click(screen.getByRole('button', { name: /look up proof/i }))
}

describe('inclusion proof download', () => {
  it('saves the proof exactly as the server sent it (same characters, not re-serialized)', async () => {
    const user = await openLedger()
    await askProof(user, '{"id": 7}')
    const proof = await screen.findByTestId('proof')
    await user.click(within(proof).getByRole('button', { name: /download proof\.json/i }))
    const sent = bodies.find((b) => b.path.endsWith('/inclusion-proofs'))?.text ?? ''
    expect(sent).toContain('\n\t"format"') // the layout of the server: tabs and line breaks
    expect(await savedText()).toBe(sent)
    expect(saved[0]?.name).toBe('proof.json')
    expect(saved[0]?.blob.type).toBe('application/json')
  })

  it('shows the proof without positions and says how to check it with the verifier', async () => {
    const user = await openLedger()
    await askProof(user, '{"id": 7}')
    const proof = await screen.findByTestId('proof')
    expect(within(proof).getByText(/entry path/i)).toBeInTheDocument()
    expect(within(proof).getByText(/transaction path/i)).toBeInTheDocument()
    expect(proof.textContent).not.toMatch(/\b(left|right)\b/)
    const cmd = within(proof).getByTestId('verify-command').textContent ?? ''
    const parsed = JSON.parse(
      bodies.find((b) => b.path.endsWith('/inclusion-proofs'))?.text ?? '{}',
    ) as { table_id: number }
    expect(cmd).toContain('kingres-verify proof --proof proof.json --digest digest.json')
    expect(cmd).toContain(`--table-id ${parsed.table_id}`)
    expect(cmd).toContain('--row-hash <hash recomputed from your own copy of the row>')
    expect(within(proof).getByText(/outside the database/i)).toBeInTheDocument()
    expect(within(proof).getByText(/separate program/i)).toBeInTheDocument()
  })
})

describe('digest document download', () => {
  it('saves a chain document with its own key order, spacing and an integer above 2^53', async () => {
    const user = await openLedger()
    await user.click(
      await screen.findByRole('button', { name: /download document of chain 1 of digest 3/i }),
    )
    const text = await savedText()
    const sent = bodies.find(
      (b) => b.path.endsWith('/ledger/digests') && b.text.includes('"items"'),
    )?.text as string
    // The saved text is a substring of what the server sent, character for character.
    expect(sent).toContain(text)
    expect(text).toContain('9007199254740993')
    expect(text).toMatch(/^\{ "\w+" : /)
    expect(text).toContain(' ,\n\t')
    expect(saved[0]?.name).toBe('digest-3-group-1.json')
  })

  it('offers the documents of a digest that was just generated', async () => {
    const user = await openLedger()
    const before = getState().ledger.digests.length
    await user.click(screen.getByRole('button', { name: /^generate digest$/i }))
    await waitFor(() => expect(getState().ledger.digests.length).toBe(before + 1))
    const id = getState().ledger.digests[0]?.id
    await user.click(
      await screen.findByRole('button', {
        name: new RegExp(`download document of chain 2 of digest ${id}$`, 'i'),
      }),
    )
    const text = await savedText()
    expect(text).toContain('9007199254740993')
    const all = bodies
      .filter((b) => b.path.endsWith('/ledger/digests'))
      .map((b) => b.text)
      .join('\n')
    expect(all).toContain(text)
  })
})

describe('refusals of createInclusionProof', () => {
  it('explains a row that is not in a block yet and offers to generate a digest', async () => {
    const user = await openLedger()
    await askProof(user, '{"id": 424242}')
    const notice = await screen.findByText(/row not in a block yet/i)
    const box = notice.closest('[data-code]') as HTMLElement
    expect(box.dataset.code).toBe('ledger_entry_pending')
    expect(box).toHaveTextContent(/generate a digest/i)
    expect(box).toHaveTextContent(/ask again/i)
    const before = getState().ledger.digests.length
    await user.click(screen.getByRole('button', { name: /generate digest and ask again/i }))
    await waitFor(() => expect(getState().ledger.digests.length).toBe(before + 1))
    // The block is closed now: the same question is answered with the proof.
    expect(await screen.findByTestId('proof')).toBeInTheDocument()
    expect(screen.queryByText(/row not in a block yet/i)).toBeNull()
  })

  it.each([
    ['{"id": 404404}', 'not_found', /hides it from your database role/i],
    ['{"id": 403403}', 'forbidden', /no privilege on this table/i],
  ])('shows %s as %s with the server text', async (key, code, text) => {
    const user = await openLedger()
    await askProof(user, key)
    expect(await screen.findByTestId('proof-hint')).toHaveTextContent(text)
    expect(document.querySelector('[data-code]')?.getAttribute('data-code')).toBe(code)
  })

  it('refuses a removed version of an append-only table with the server explanation', async () => {
    const user = await openLedger()
    await user.selectOptions(screen.getByLabelText('Entry'), 'D')
    await user.type(screen.getByLabelText(/transaction id of the removed version/i), '48000123')
    await askProof(user, '{"id": 7}')
    expect(await screen.findByText(/it has no removed versions/i)).toBeInTheDocument()
  })

  it('keeps the calm notice for an operation the server does not implement yet', async () => {
    mockConfig.unsupported.add('createInclusionProof')
    const user = await openLedger()
    await askProof(user, '{"id": 7}')
    expect(await screen.findByText(/not available on this server version yet/i)).toBeInTheDocument()
  })
})

describe('verification records', () => {
  it('pages newest first with a cursor', async () => {
    const user = await openLedger()
    const table = await screen.findByRole('table', { name: 'Verifications' })
    const ids = () =>
      within(table)
        .getAllByRole('rowheader')
        .map((h) => Number(h.textContent))
    await waitFor(() => expect(ids().length).toBe(10))
    expect(ids()[0]).toBe(16)
    await user.click(screen.getByRole('button', { name: /load more verifications/i }))
    await waitFor(() => expect(ids().length).toBe(12))
    expect(ids().at(-1)).toBe(5)
    expect(screen.queryByRole('button', { name: /load more verifications/i })).toBeNull()
  })

  it('shows the findings of a failed run, including the kind "other", and links its task', async () => {
    const user = await openLedger()
    const row = (await screen.findByRole('rowheader', { name: '12' })).closest('tr') as HTMLElement
    await user.click(within(row).getByRole('button', { name: /details/i }))
    const dlg = await screen.findByRole('dialog', { name: /verification 12/i })
    expect(await within(dlg).findByText(/other check/i)).toBeInTheDocument()
    expect(within(dlg).getByText(/block hash differs/i)).toBeInTheDocument()
    expect(within(dlg).getByText(/ledger_check_x/)).toBeInTheDocument()
    expect(within(dlg).getByRole('link', { name: 'Task 92' })).toHaveAttribute(
      'href',
      '/tasks?task=92',
    )
  })

  it('says so when a run was recorded without a task', async () => {
    const user = await openLedger()
    const row = (await screen.findByRole('rowheader', { name: '8' })).closest('tr') as HTMLElement
    await user.click(within(row).getByRole('button', { name: /details/i }))
    const dlg = await screen.findByRole('dialog', { name: /verification 8/i })
    expect(await within(dlg).findByText(/recorded without a task/i)).toBeInTheDocument()
    expect(within(dlg).queryByRole('link')).toBeNull()
  })

  it('lists a run that was started, with its task', async () => {
    const user = await openLedger()
    await user.click(screen.getByRole('button', { name: /start verification/i }))
    await waitFor(() => expect(getState().ledger.verifications[0]?.state).toBe('passed'), {
      timeout: 4000,
    })
    const id = getState().ledger.verifications[0]?.id as number
    expect(getState().ledger.verifications[0]?.taskId).toBeGreaterThan(0)
    expect(await screen.findByRole('rowheader', { name: String(id) })).toBeInTheDocument()
  })

  it('says there are no verifications for a database without any', async () => {
    getState().ledger.verifications.length = 0
    await openLedger()
    expect(await screen.findByText(/no verifications yet/i)).toBeInTheDocument()
  })
})

describe('the link from a verification to its task', () => {
  it('opens the task of a run that was started', async () => {
    const user = await openLedger()
    await user.click(screen.getByRole('button', { name: /start verification/i }))
    await waitFor(() => expect(getState().ledger.verifications[0]?.state).toBe('passed'), {
      timeout: 4000,
    })
    const taskId = getState().ledger.verifications[0]?.taskId as number
    const link = await screen.findByRole('link', { name: `Task ${taskId}` })
    await user.click(link)
    const dlg = await screen.findByRole('dialog', { name: `Task ${taskId}` })
    expect(await within(dlg).findByText(/check block hashes/i)).toBeInTheDocument()
  })

  it('says that a task which no longer exists is gone', async () => {
    await renderApp('/tasks?task=4242')
    const dlg = await screen.findByRole('dialog', { name: 'Task 4242' })
    expect(await within(dlg).findByText(/does not exist or was removed/i)).toBeInTheDocument()
  })
})
