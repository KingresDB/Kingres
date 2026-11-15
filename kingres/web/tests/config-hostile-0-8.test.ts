// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.8.0: every text of the Configuration feature that the server sends (parameter names, categories,
// descriptions, values, enum values, PostgreSQL's error texts, server ids, group and database names, zone names) is
// chosen by someone who may set a placeholder setting or create a database, so it is shown as text with every hidden
// character visible, is never markup, never an id, and never the target of a label.
import { afterEach, describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { getState } from '@/mocks/http'
import { mockServer } from '@/mocks/server'
import { failNextApply } from '@/mocks/config'
import { HOSTILE, seedHostile } from '@/mocks/hostile'
import { renderApp } from './helpers'

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
  expect(document.querySelector('img[src="x"]')).toBeNull()
  expect(document.querySelector('svg[onload]')).toBeNull()
  expect((window as unknown as { __pwn?: number }).__pwn).toBeUndefined()
  expect(document.body.textContent).not.toContain('first line\n2026')
}

/** An id or the target of a label never holds text from the server. */
function expectNoServerTextInIds(): void {
  for (const el of Array.from(document.querySelectorAll('[id]')))
    expect(el.id, el.id).toMatch(/^[A-Za-z0-9_-]+$/)
  for (const label of Array.from(document.querySelectorAll('label[for]')))
    expect(label.getAttribute('for'), label.textContent ?? '').toMatch(/^[A-Za-z0-9_-]+$/)
  for (const el of Array.from(
    document.querySelectorAll('[aria-labelledby], [aria-describedby], [aria-controls]'),
  ))
    for (const a of ['aria-labelledby', 'aria-describedby', 'aria-controls'])
      for (const id of (el.getAttribute(a) ?? '').split(' ').filter(Boolean))
        expect(id).toMatch(/^[A-Za-z0-9_-]+$/)
}

async function load(path: string, user = 'admin') {
  seedHostile(getState())
  return renderApp(path, { user })
}

afterEach(() => {
  delete (window as unknown as { __pwn?: number }).__pwn
})

describe('the Parameters tab with hostile parameters', () => {
  it('shows the names, categories, descriptions and values of the servers as text, with hidden characters visible', async () => {
    await load('/configuration?q=img')
    const row = within(await screen.findByTestId(`param-${HOSTILE.markup}`))
    // The markup is on the page as text.
    expect(row.getAllByText(HOSTILE.markup).length).toBeGreaterThan(0)
    // The category begins with a right-to-left override: it is shown as a visible mark, not obeyed.
    expect(row.getAllByRole('img', { name: /hidden character U\+202E/ }).length).toBeGreaterThan(0)
    // 5000 characters are folded, with a way to see them all.
    expect(row.getAllByRole('button', { name: /Show all 5000 characters/ }).length).toBeGreaterThan(
      0,
    )
    // The look-alike letters of a value are flagged.
    expect(row.getByText(/аdmin/)).toBeInTheDocument()
    expectSafePage()
    expectNoServerTextInIds()
  })

  it('shows the error text of PostgreSQL with the value it quotes as text', async () => {
    await load('/configuration?q=img&state=failed')
    const row = within(await screen.findByTestId(`param-${HOSTILE.markup}`))
    expect(row.getByText('Failed')).toBeInTheDocument()
    expect(row.getAllByText(/invalid value for parameter/).length).toBeGreaterThan(0)
    expectSafePage()
  })

  it('shows the values of an enum with hostile members in the details of the row', async () => {
    const user = userEvent.setup()
    await load(`/configuration?q=${encodeURIComponent('hostile.')}`)
    const name = `hostile.${HOSTILE.breakout}`
    const row = within(await screen.findByTestId(`param-${name}`))
    await user.click(row.getByText('4 values'))
    expect(row.getAllByText(HOSTILE.markup).length).toBeGreaterThan(0)
    expectSafePage()
  })

  it('shows the filters, the paging and the empty result safely', async () => {
    await load(
      `/configuration?q=${encodeURIComponent(HOSTILE.markup)}x&category=${encodeURIComponent(HOSTILE.reversed)}`,
    )
    expect(await screen.findByText('No parameter matches these filters')).toBeInTheDocument()
    expectSafePage()
  })
})

describe('the dialog with hostile parameters', () => {
  async function open(name: string) {
    const user = userEvent.setup()
    await load(`/configuration?q=${encodeURIComponent(name)}`)
    const row = await screen.findByTestId(`param-${name}`)
    await user.click(within(row).getByRole('button', { name: /^Edit/ }))
    const dialog = await screen.findByRole('dialog')
    await within(dialog).findByRole('group', { name: 'Level' })
    return { user, d: within(dialog) }
  }

  it('shows the name, the description, the current value and the scopes as text', async () => {
    const { user, d } = await open(HOSTILE.markup)
    expect(d.getByRole('heading', { level: 2 })).toHaveTextContent(`Edit ${HOSTILE.markup}`)
    expect(d.getByLabelText('Value')).toHaveValue(HOSTILE.long)
    // The server groups of the hostile seed are options of the scope.
    await user.click(d.getByRole('radio', { name: 'Server group' }))
    const scope = d.getByRole('combobox', { name: 'Server group' })
    expect(within(scope).getAllByRole('option').length).toBe(4)
    await user.click(d.getByRole('radio', { name: 'Database' }))
    expect(
      within(d.getByRole('combobox', { name: 'Database' })).getAllByRole('option').length,
    ).toBeGreaterThan(5)
    expectSafePage()
    expectNoServerTextInIds()
  })

  it('shows an override of a hostile group, the current value there, as text', async () => {
    const { user, d } = await open(HOSTILE.markup)
    await user.click(d.getByRole('radio', { name: 'Server group' }))
    await user.selectOptions(d.getByRole('combobox', { name: 'Server group' }), HOSTILE.markup)
    expect(d.getByTestId('current-override')).toHaveTextContent('Set here now:')
    expectSafePage()
  })

  it('sends a hostile value as it is typed and keeps it out of the markup', async () => {
    let sent: unknown
    mockServer.use(
      http.post('*/api/v1/config/changes/preview', async ({ request }) => {
        sent = await request.json()
        return HttpResponse.json({
          valid: false,
          problems: [
            {
              index: 0,
              pointer: '/changes/0/value',
              code: 'config_value_invalid',
              message: HOSTILE.markup,
            },
          ],
          effects: [],
          restartServerIds: [],
          restartOrder: [],
          restartOrderConflict: null,
        })
      }),
    )
    const { user, d } = await open(HOSTILE.markup)
    const input = d.getByLabelText('Value')
    await user.clear(input)
    await user.click(input)
    await user.paste(HOSTILE.breakout)
    const problems = await d.findByTestId('preview-problems')
    expect(problems).toHaveTextContent(HOSTILE.markup)
    expect(sent).toMatchObject({
      changes: [{ name: HOSTILE.markup, level: 'cluster', scopeId: null, value: HOSTILE.breakout }],
    })
    expectSafePage()
  })

  it('shows hostile server ids and the error text of a failed server in the state table', async () => {
    failNextApply(
      getState(),
      '10.0.0.12',
      `invalid value for parameter "${HOSTILE.markup}": "${HOSTILE.reversed}"
${HOSTILE.long}`,
    )
    const user = userEvent.setup()
    await load('/configuration?q=work_mem')
    const row = await screen.findByTestId('param-work_mem')
    await user.click(within(row).getByRole('button', { name: 'Edit work_mem' }))
    const d = within(await screen.findByRole('dialog'))
    await d.findByRole('group', { name: 'Level' })
    const input = d.getByLabelText('Value')
    await user.clear(input)
    await user.type(input, '16MB')
    await waitFor(() =>
      expect(d.getByRole('button', { name: /^Apply/ })).not.toHaveAttribute('aria-disabled'),
    )
    await user.click(d.getByRole('button', { name: /^Apply/ }))
    const failed = await d.findByTestId('server-state-10.0.0.12')
    await waitFor(() => expect(failed).toHaveTextContent('invalid value for parameter'))
    expectSafePage()
    expectNoServerTextInIds()
  })
})

describe('the other tabs, the dashboard and the server page with hostile data', () => {
  it('shows hostile values, scopes and names in the history', async () => {
    await load('/configuration?tab=changes')
    await screen.findByTestId('change-6')
    expect(screen.getAllByText(HOSTILE.markup).length).toBeGreaterThan(0)
    expect(
      screen.getAllByRole('button', { name: /Show all 5000 characters/ }).length,
    ).toBeGreaterThan(0)
    expectSafePage()
  })

  it('shows hostile database names and a hostile group name on the Time tab', async () => {
    await load('/configuration?tab=time')
    await screen.findByTestId('measured-at')
    expect(await screen.findAllByText(/U\+202E/)).not.toHaveLength(0)
    expectSafePage()
    expectNoServerTextInIds()
  })

  it('shows a hostile zone name, server id and database name in the clock tables', async () => {
    seedHostile(getState())
    mockServer.use(
      http.get('*/api/v1/config/time', () =>
        HttpResponse.json({
          measuredAt: new Date().toISOString(),
          referenceServerId: HOSTILE.reversed,
          maxDifferenceMs: 1000,
          servers: [
            {
              serverId: HOSTILE.reversed,
              reachable: true,
              clock: new Date().toISOString(),
              differenceMs: 0,
              uncertaintyMs: 0,
              roundTripMs: 0,
              osTimeZone: HOSTILE.markup,
              timeZone: HOSTILE.zeroWidth,
              logTimeZone: HOSTILE.long,
            },
            {
              serverId: '..',
              reachable: false,
              clock: null,
              differenceMs: null,
              uncertaintyMs: null,
              roundTripMs: null,
              osTimeZone: null,
              timeZone: null,
              logTimeZone: null,
            },
          ],
          databases: [{ name: HOSTILE.markup, timeZone: HOSTILE.reversed }],
        }),
      ),
    )
    await renderApp('/configuration?tab=time')
    await screen.findByTestId('measured-at')
    expect(screen.getAllByText(HOSTILE.markup).length).toBeGreaterThan(0)
    expectSafePage()
    expectNoServerTextInIds()
  })

  it('shows hostile server ids in the dashboard card', async () => {
    mockServer.use(
      http.get('*/api/v1/config/summary', () =>
        HttpResponse.json({
          revision: 1,
          serversPendingRestart: [HOSTILE.reversed],
          differingInGroup: [HOSTILE.markup],
          failedServerIds: [HOSTILE.markup],
          waitingServerIds: [],
          largestClockDifference: { serverId: HOSTILE.reversed, differenceMs: -5000 },
          maxDifferenceMs: 1000,
        }),
      ),
    )
    await load('/dashboard')
    const card = within(await screen.findByTestId('config-card'))
    await waitFor(() => expect(card.getByTestId('cfg-clock')).toHaveTextContent('5.00 s'))
    expect(card.getByTestId('cfg-clock')).toHaveTextContent(/U\+202E/)
    expectSafePage()
  })

  it('shows hostile names that wait for a restart on the page, in the dialog and on the server page', async () => {
    seedHostile(getState())
    const second = getState().config.servers['10.0.0.12']!
    second.pending[HOSTILE.markup] = HOSTILE.reversed
    second.pending[`hostile.${HOSTILE.breakout}`] = HOSTILE.zeroWidth
    const user = userEvent.setup()
    await renderApp('/configuration')
    await screen.findByTestId('pending-note')
    await user.click(screen.getByRole('button', { name: /restart pending instances/i }))
    const d = within(await screen.findByRole('dialog', { name: 'Restart pending instances' }))
    expect((await d.findByTestId('waiting-servers')).textContent).toContain('shared_buffers')
    expect(d.getAllByText(HOSTILE.markup).length).toBeGreaterThan(0)
    expectSafePage()
    expectNoServerTextInIds()
  })

  it('shows hostile text on the section of the server page', async () => {
    seedHostile(getState())
    getState().config.servers['10.0.0.12']!.failed[HOSTILE.markup] =
      `${HOSTILE.reversed}\n${HOSTILE.long}`
    await renderApp('/servers/10.0.0.12')
    const section = within(await screen.findByTestId('server-config'))
    expect(await section.findByTestId('config-failed')).toHaveTextContent(HOSTILE.markup)
    expectSafePage()
    expectNoServerTextInIds()
  })

  it('shows a hostile alert about a clock safely', async () => {
    seedHostile(getState())
    getState().alerts.unshift({
      id: 990,
      kind: 'clockDifference',
      severity: 'warning',
      state: 'open',
      title: HOSTILE.markup,
      message: HOSTILE.newline,
      serverId: HOSTILE.reversed,
      databaseName: null,
      raisedAt: new Date().toISOString(),
      subject: {
        serverId: HOSTILE.reversed,
        behindServerId: null,
        shardGroupId: null,
        databaseName: null,
        aspect: null,
      },
    })
    await renderApp('/alerts')
    expect((await screen.findAllByText(HOSTILE.markup)).length).toBeGreaterThan(0)
    expectSafePage()
  })
})
