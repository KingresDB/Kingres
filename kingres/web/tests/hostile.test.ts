// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// W-05 and W-17: names and messages chosen by a low-privileged database user (markup, bidirectional
// controls, zero-width and control characters, look-alike letters, very long values) are shown as
// text with every hidden character visible, on the list and the detail pages, and in dialogs.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { getState } from '@/mocks/http'
import { mockServer } from '@/mocks/server'
import { HOSTILE, seedHostile } from '@/mocks/hostile'
import { renderApp, settled } from './helpers'

/** Characters that draw nothing or reorder text: none may reach the page raw. */
const RAW = new RegExp(
  `[${[
    '202a-202e',
    '2066-2069',
    '200b-200f',
    '2028-2029',
    '2060',
    'feff',
    '0000-0008',
    '000b-001f',
    '007f',
  ]
    .map((r) =>
      r
        .split('-')
        .map((h) => `\\u${h}`)
        .join('-'),
    )
    .join('')}]`,
)

/** Everything a person or a screen reader can get from the page: text and the usual attributes. */
function visibleStrings(): string[] {
  const out: string[] = [document.body.textContent ?? '']
  for (const el of Array.from(document.body.querySelectorAll('*'))) {
    for (const attr of ['title', 'aria-label', 'placeholder', 'alt']) {
      const v = el.getAttribute(attr)
      if (v) out.push(v)
    }
  }
  return out
}

function expectSafePage(): void {
  for (const text of visibleStrings()) expect(text).not.toMatch(RAW)
  // Markup stayed text: no element was made from it, nothing ran.
  expect(document.querySelector('img[src="x"]')).toBeNull()
  expect(document.querySelector('svg[onload]')).toBeNull()
  expect((window as unknown as { __pwn?: number }).__pwn).toBeUndefined()
  expect(document.body.textContent).not.toContain('first line\n2026')
}

async function load(path: string, user = 'admin') {
  seedHostile(getState())
  const app = await renderApp(path, { user })
  await settled()
  return app
}

afterEach(() => {
  delete (window as unknown as { __pwn?: number }).__pwn
})

describe('list pages show hostile names safely', () => {
  const pages: [string, RegExp, string?][] = [
    ['/databases', /U\+202E/],
    ['/alerts', /U\+202E/],
    ['/logs', /U\+001B/],
    ['/servers', /U\+202E/],
    ['/tasks', /U\+202E/],
    ['/dashboard', /U\+202E|<img src=x/],
    ['/ledger', /Status of/],
    ['/ledger?tab=digests', /Aggregate checkpoints/],
    ['/ledger?tab=anchoring', /U\+202E/],
    ['/ledger?tab=clients', /U\+202E/],
    ['/ledger?tab=signing', /U\+202E/],
    ['/ledger?tab=publication', /U\+202E/],
    ['/shards', /Bucket map/],
    ['/replication', /Replication/],
    ['/rebalance', /Rebalance/],
  ]
  for (const [path, expected] of pages) {
    it(path, async () => {
      await load(path)
      await waitFor(() => expect(document.body.textContent).toMatch(expected))
      expectSafePage()
    })
  }

  it('/security: roles, access, network access and the audit log', async () => {
    const user = userEvent.setup()
    await load('/security')
    await waitFor(() => expect(document.body.textContent).toMatch(/U\+202E/))
    expect(document.body.textContent).toContain(HOSTILE.markup)
    expectSafePage()
    for (const tab of ['Access', 'Network access', 'Audit log']) {
      await user.click(screen.getByRole('tab', { name: tab }))
      await settled()
      if (tab === 'Audit log')
        await waitFor(() => expect(document.body.textContent).toMatch(/U\+202E/))
      expectSafePage()
    }
  })

  it('/workbench: columns, cells and an error message', async () => {
    const user = userEvent.setup()
    await load('/workbench')
    const sql = screen.getByLabelText(/^sql/i)
    await user.clear(sql)
    await user.type(sql, 'select * from hostile')
    await user.click(screen.getByRole('button', { name: /^run$/i }))
    const result = await screen.findByTestId('result')
    expect(result.textContent).toMatch(/U\+202E/)
    expect(result.textContent).toMatch(/U\+001B/)
    expect(within(result).getAllByRole('columnheader')).toHaveLength(4)
    expectSafePage()
  })
})

describe('detail pages and dialogs show hostile names safely', () => {
  it('a database page lists hostile tables, schemas and keeps tables with the same text apart', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined)
    await load('/databases/shop')
    await waitFor(() => expect(document.body.textContent).toMatch(/U\+202E/))
    // The page has a second table since 0.15.0 (the schemas); this one is the tables.
    expect(
      within(screen.getByRole('table', { name: /tables of the database/i })).getAllByRole(
        'rowheader',
      ).length,
    ).toBeGreaterThan(10)
    expectSafePage()
    expect(warn.mock.calls.flat().join(' ')).not.toMatch(/duplicate keys/i)
    warn.mockRestore()
  })

  it('a database with a hostile name opens, and its title shows the escapes', async () => {
    await load(`/databases/${encodeURIComponent(HOSTILE.reversed)}`)
    await waitFor(() =>
      expect(screen.getByRole('heading', { level: 1 }).textContent).toMatch(/U\+202E/),
    )
    expectSafePage()
  })

  it('a database named ".." is listed but cannot be opened by URL, and no request goes to another URL', async () => {
    const urls: string[] = []
    mockServer.events.on('request:start', ({ request }) => urls.push(new URL(request.url).pathname))
    await load('/databases')
    await waitFor(() => expect(document.body.textContent).toContain('..'))
    expect(screen.queryByRole('link', { name: '..' })).not.toBeInTheDocument()
    expect(screen.getByText(/cannot be opened by address/i)).toBeInTheDocument()
    expect(urls).not.toContain('/api/v1/')
  })

  it('dropping a role with a reversed name shows the escaped name and needs the real one', async () => {
    const user = userEvent.setup()
    await load('/security')
    const row = (await screen.findByText(/U\+202E/, { selector: 'th *' })).closest(
      'tr',
    ) as HTMLElement
    await user.click(within(row).getByRole('button', { name: /drop/i }))
    const dialog = await screen.findByRole('dialog', { name: /drop role.*U\+202E/i })
    expect(dialog.textContent).not.toMatch(RAW)
    const drop = within(dialog).getByRole('button', { name: /^drop role$/i })
    // What is drawn is not what has to be typed: typing the visible text does not confirm.
    await user.type(within(dialog).getByLabelText(/to confirm/i), 'nimda')
    expect(drop).toBeDisabled()
    await fireEvent.update(within(dialog).getByLabelText(/to confirm/i), HOSTILE.reversed)
    expect(drop).toBeEnabled()
    expect(within(dialog).getByRole('button', { name: /copy the name/i })).toBeInTheDocument()
  })

  it('flags a name that mixes scripts and a name with a hidden character', async () => {
    await load('/security')
    await waitFor(() => expect(document.body.textContent).toMatch(/mixed scripts/i))
    expect(document.body.textContent).toMatch(/hidden characters/i)
  })

  it('distributing a table colocates with the table that was chosen, not one with the same text', async () => {
    const user = userEvent.setup()
    await load('/databases/shop')
    const row = (await screen.findAllByRole('rowheader', { name: /U\+202E/ }))[0]!.closest(
      'tr',
    ) as HTMLElement
    await user.click(within(row).getByRole('button', { name: /distribute table/i }))
    const dialog = await screen.findByRole('dialog', { name: /distribute/i })
    const target = await within(dialog).findByLabelText(/colocate with/i)
    const options = within(target as HTMLElement).getAllByRole('option')
    const same = options.filter((o) => o.textContent === 'x.y.z')
    expect(same).toHaveLength(2)
    const bodies: unknown[] = []
    mockServer.events.on('request:start', async ({ request }) => {
      if (request.method === 'PUT' && request.url.endsWith('/distribution'))
        bodies.push(await request.clone().json())
    })
    await user.selectOptions(target, same[1] as HTMLElement)
    await user.click(within(dialog).getByRole('button', { name: /^distribute table$/i }))
    await waitFor(() => expect(bodies).toHaveLength(1))
    expect(bodies[0]).toMatchObject({ colocateWith: { schema: 'x.y', table: 'z' } })
  })

  it('the server group names and descriptions are shown safely, and delete names the group', async () => {
    const user = userEvent.setup()
    await load('/servers')
    await waitFor(() => expect(document.body.textContent).toMatch(/U\+0001B|U\+001B/))
    const button = (await screen.findAllByRole('button', { name: /^delete$/i }))[0]!
    await user.click(button)
    const dialog = await screen.findByRole('dialog', { name: /delete server group/i })
    expect(dialog.textContent).not.toMatch(RAW)
    expectSafePage()
  })

  it('an error from the server that contains hidden characters is shown with them visible', async () => {
    mockServer.use(
      http.get('*/api/v1/servers', () =>
        HttpResponse.json(
          {
            type: 'about:blank',
            title: 'x',
            status: 422,
            code: 'sql_error',
            detail: `relation "${HOSTILE.reversed}" does not exist`,
          },
          { status: 422 },
        ),
      ),
    )
    await renderApp('/servers')
    await screen.findByText(/U\+202E/)
    expectSafePage()
  })
})
