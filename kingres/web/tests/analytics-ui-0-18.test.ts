// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The analytics console (contract 0.18.0, phase 12): the approximate-aggregates card and dialog of a database, the Storage column
// and filter of the tables list, the table page (storage, partitions with their format, the time-partitioning policy, its runs),
// the dialogs (convert storage, time-partitioning policy with its preview, run now, remove), the alert link, and the plan
// summary of the workbench. Every dialog follows the preview-then-confirm rule where it deletes or locks.
import { beforeEach, describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { axe } from 'vitest-axe'
import { endpoints } from '@/api/endpoints'
import { getState } from '@/mocks/http'
import { mockConfig } from '@/mocks/state'
import { applySwitches } from '@/mocks/switches'
import { mockServer } from '@/mocks/server'
import { createPinia } from 'pinia'
import { renderApp, signInAs } from './helpers'

const sw = (q: string) => applySwitches(new URLSearchParams(q), getState())
type User = ReturnType<typeof userEvent.setup>
const db = '/databases/shop'
const table = (name: string) => `/databases/shop/tables/public/${name}`
const dialogOf = async (name: RegExp) => within(await screen.findByRole('dialog', { name }))
const enabled = async (b: HTMLElement) => waitFor(() => expect(b).toBeEnabled())
const bodies: unknown[] = []
let capture: { method: string; suffix: string } | null = null
mockServer.events.on('request:start', async ({ request }) => {
  const url = new URL(request.url)
  // A request with dryRun only asks; it is not a change.
  if (
    capture &&
    request.method === capture.method &&
    url.pathname.endsWith(capture.suffix) &&
    url.searchParams.get('dryRun') !== 'true'
  )
    bodies.push(await request.clone().json())
})
/** Records the bodies of the requests of one method and path suffix, from now on (one recorder, so that tests do not add up). */
const recordBodies = (method: string, suffix: string) => {
  bodies.length = 0
  capture = { method, suffix }
}
beforeEach(() => {
  capture = null
  bodies.length = 0
})
/** Signs in for a test that calls the API before it renders the page. */
const signedIn = () => signInAs(createPinia())
const clear = async (user: User, el: HTMLElement, text: string) => {
  await user.clear(el)
  if (text) await user.type(el, text)
}

describe('the approximate-aggregates card', () => {
  it('says off, with the text that explains it, and every server not installed', async () => {
    await renderApp(db)
    const card = within(await screen.findByTestId('aggregates-card'))
    expect(await card.findByTestId('aggregates-facts')).toHaveTextContent(/state\s*○\s*off/i)
    expect(card.getByTestId('aggregates-mode')).toHaveTextContent(/^\s*exact\s*$/i)
    expect(card.getByText(/adds the sketch types hll, topn and tdigest/i)).toBeInTheDocument()
    expect(card.getByText(/the exact count\(DISTINCT\) is the default/i)).toBeInTheDocument()
    const rows = within(card.getByTestId('aggregates-servers')).getAllByRole('row')
    expect(rows).toHaveLength(getState().servers.length + 1)
    expect(within(rows[1]!).getByText('Not installed')).toBeInTheDocument()
  })

  it('says on, with the version, the mode and the error rate, and each server installed', async () => {
    sw('analytics=on')
    await renderApp(db)
    const card = within(await screen.findByTestId('aggregates-card'))
    await waitFor(() => expect(card.getByTestId('aggregates-facts')).toHaveTextContent(/on/i))
    expect(card.getByTestId('aggregates-facts')).toHaveTextContent(/extension version\s*1\.0/i)
    expect(card.getByTestId('aggregates-mode')).toHaveTextContent(
      /approximate \(error rate 1 percent\)/i,
    )
    expect(card.getAllByText('Installed')).toHaveLength(getState().servers.length)
  })

  it('shows a server with another version, and one that is not reachable, as a state in words', async () => {
    sw('analytics=on')
    const agg = getState().analytics!.aggregates.shop!
    const [a, b] = Object.keys(agg.versions)
    agg.versions[a!] = '0.9'
    agg.versions[b!] = '1.0'
    await renderApp(db)
    const card = within(await screen.findByTestId('aggregates-card'))
    expect((await card.findAllByText('Different version')).length).toBeGreaterThan(0)
    expect(card.getByTestId('aggregates-facts')).toHaveTextContent(/extension version\s*n\/a/i)
  })

  it('a server id with a hidden character is shown with the character visible', async () => {
    getState().servers[1]!.id = 'srv‮evil'
    await renderApp(db)
    const card = within(await screen.findByTestId('aggregates-card'))
    expect(await card.findByRole('img', { name: /hidden character U\+202E/i })).toBeInTheDocument()
  })

  it.each([
    ['viewer', /kingres_admin/],
    ['ops', /kingres_admin/],
  ])(
    '%s sees the whole card and a Configure button that says why it is off',
    async (user, reason) => {
      await renderApp(db, { user })
      const card = within(await screen.findByTestId('aggregates-card'))
      await card.findByTestId('aggregates-facts')
      const button = card.getByRole('button', { name: /^configure/i })
      await waitFor(() => expect(button).toHaveAttribute('aria-disabled', 'true'))
      expect(button).toHaveTextContent(reason)
    },
  )

  it('a server that does not know the operation shows calm information, not a failure', async () => {
    sw('analytics=old')
    await renderApp(db)
    const card = within(await screen.findByTestId('aggregates-card'))
    await waitFor(() =>
      expect(
        card.getByText(/not (yet )?available|not implemented|not supported/i),
      ).toBeInTheDocument(),
    )
    expect(card.getByRole('button', { name: /^configure/i })).toHaveAttribute(
      'aria-disabled',
      'true',
    )
  })

  it('has no accessibility violations', async () => {
    sw('analytics=on')
    const { container } = await renderApp(db)
    await screen.findByTestId('aggregates-facts')
    expect(await axe(container)).toHaveNoViolations()
  })
})

describe('the approximate-aggregates dialog', () => {
  async function open(user: User) {
    await renderApp(db)
    const button = await screen.findByTestId('aggregates-configure')
    await enabled(button)
    await user.click(button)
    return dialogOf(/^approximate aggregates of shop/i)
  }

  it('enables the approximate mode with an error rate, follows the five steps, and the card says on', async () => {
    const user = userEvent.setup()
    mockConfig.taskDurationMs = 80
    recordBodies('PUT', '/approximate-aggregates')
    const dialog = await open(user)
    await user.click(dialog.getByLabelText(/enable on this database/i))
    await user.selectOptions(dialog.getByLabelText('count(DISTINCT)'), 'approximate')
    expect(dialog.getByTestId('aa-consequence')).toHaveTextContent(
      /can differ from exact counts by the error rate/i,
    )
    await clear(user, dialog.getByLabelText(/error rate \(percent\)/i), '2')
    await user.click(dialog.getByRole('button', { name: /^save and install$/i }))
    expect(await dialog.findByText('Finished.')).toBeInTheDocument()
    expect(dialog.getAllByText('Check the servers').length).toBeGreaterThan(0)
    expect(dialog.getAllByText('Verify versions and hashes').length).toBeGreaterThan(0)
    expect(bodies).toEqual([{ enabled: true, countDistinct: 'approximate', errorRate: 0.02 }])
    await user.click(dialog.getByRole('button', { name: /^close$/i }))
    const card = within(screen.getByTestId('aggregates-card'))
    await waitFor(() =>
      expect(card.getByTestId('aggregates-mode')).toHaveTextContent(
        /approximate \(error rate 2 percent\)/i,
      ),
    )
  })

  it('an error rate outside 0.05 to 10 is refused before anything is sent', async () => {
    const user = userEvent.setup()
    recordBodies('PUT', '/approximate-aggregates')
    const dialog = await open(user)
    await user.click(dialog.getByLabelText(/enable on this database/i))
    await user.selectOptions(dialog.getByLabelText('count(DISTINCT)'), 'approximate')
    for (const bad of ['0', '11', '0.01']) {
      await clear(user, dialog.getByLabelText(/error rate \(percent\)/i), bad)
      await user.click(dialog.getByRole('button', { name: /^save and install$/i }))
      expect((await dialog.findAllByText(/use a number from 0\.05 to 10/i)).length).toBeGreaterThan(
        0,
      )
    }
    expect(bodies).toEqual([])
    await clear(user, dialog.getByLabelText(/error rate \(percent\)/i), '0.05')
    await user.click(dialog.getByRole('button', { name: /^save and install$/i }))
    await dialog.findByText('Finished.')
    expect(bodies).toEqual([{ enabled: true, countDistinct: 'approximate', errorRate: 0.0005 }])
  })

  it('the exact mode sends no error rate; the mode select is off while the setting is off', async () => {
    const user = userEvent.setup()
    mockConfig.taskDurationMs = 60
    recordBodies('PUT', '/approximate-aggregates')
    const dialog = await open(user)
    expect(dialog.getByLabelText('count(DISTINCT)')).toBeDisabled()
    expect(dialog.getByTestId('aa-off-note')).toHaveTextContent(/never removes the types/i)
    await user.click(dialog.getByLabelText(/enable on this database/i))
    expect(dialog.queryByLabelText(/error rate/i)).not.toBeInTheDocument()
    await user.click(dialog.getByRole('button', { name: /^save and install$/i }))
    await dialog.findByText('Finished.')
    expect(bodies).toEqual([{ enabled: true, countDistinct: 'exact' }])
  })

  it('turning it off sends only enabled false and says the types stay', async () => {
    sw('analytics=on')
    const user = userEvent.setup()
    mockConfig.taskDurationMs = 60
    recordBodies('PUT', '/approximate-aggregates')
    await renderApp(db)
    await waitFor(() => expect(screen.getByTestId('aggregates-facts')).toHaveTextContent(/on/i))
    await user.click(await screen.findByTestId('aggregates-configure'))
    const dialog = await dialogOf(/^approximate aggregates of shop/i)
    expect(dialog.getByLabelText(/enable on this database/i)).toBeChecked()
    await user.click(dialog.getByLabelText(/enable on this database/i))
    await user.click(dialog.getByRole('button', { name: /^turn off$/i }))
    await dialog.findByText('Finished.')
    expect(bodies).toEqual([{ enabled: false }])
    await user.click(dialog.getByRole('button', { name: /^close$/i }))
    await waitFor(() =>
      expect(screen.getByTestId('aggregates-mode')).toHaveTextContent(/^\s*exact\s*$/i),
    )
  })

  it.each([
    [
      'sketch:missing',
      'The extension is not available on a server',
      /has no package for kingres_sketch/i,
    ],
    ['sketch:mismatch', 'The servers run different versions of the extension', /1\.0 and 0\.9/],
  ])(
    '%s is shown with its own explanation and nothing is changed',
    async (knob, heading, detail) => {
      sw(`analytics=${knob}`)
      const user = userEvent.setup()
      const dialog = await open(user)
      await user.click(dialog.getByLabelText(/enable on this database/i))
      await user.click(dialog.getByRole('button', { name: /^save and install$/i }))
      expect(await dialog.findByText(heading)).toBeInTheDocument()
      expect(dialog.getByText(detail)).toBeInTheDocument()
      expect(getState().analytics!.aggregates.shop?.enabled ?? false).toBe(false)
    },
  )

  it('a hash that differs ends the task failed, with the reason and its code', async () => {
    sw('analytics=sketch:hash')
    const user = userEvent.setup()
    mockConfig.taskDurationMs = 80
    const dialog = await open(user)
    await user.click(dialog.getByLabelText(/enable on this database/i))
    await user.click(dialog.getByRole('button', { name: /^save and install$/i }))
    expect(
      await dialog.findByText(/hash differently/i, {}, { timeout: 30_000 }),
    ).toBeInTheDocument()
    expect(getState().analytics!.aggregates.shop?.enabled ?? false).toBe(false)
  })

  it('has no accessibility violations', async () => {
    const user = userEvent.setup()
    await open(user)
    expect(await axe(screen.getByRole('dialog'))).toHaveNoViolations()
  })
})

describe('the Storage column of the tables list', () => {
  it('shows Row, Mixed (n columnar of m) and the filter', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    await renderApp(db)
    const list = within(await screen.findByRole('table', { name: /tables of the database/i }))
    await list.findByRole('link', { name: 'public.metrics' })
    const row = (name: string) => list.getByRole('link', { name }).closest('tr')!
    expect(within(row('public.clicks')).getByTestId('storage-cell')).toHaveTextContent(/^○Row$/)
    expect(within(row('public.metrics')).getByTestId('storage-mixed')).toHaveTextContent(
      'Mixed (2 columnar of 10)',
    )
    expect(within(row('public.metrics')).getByTestId('storage-ratio')).toHaveTextContent('6.2 x')
    await user.selectOptions(screen.getByTestId('storage-filter'), 'columnar')
    await waitFor(() =>
      expect(list.queryByRole('link', { name: 'public.clicks' })).not.toBeInTheDocument(),
    )
    expect(list.getByRole('link', { name: 'public.metrics' })).toBeInTheDocument()
    await user.selectOptions(screen.getByTestId('storage-filter'), 'heap')
    expect(await list.findByRole('link', { name: 'public.clicks' })).toBeInTheDocument()
  })

  it('a table whose storage is not known says so, never a format', async () => {
    mockServer.use(
      ...[
        (await import('msw')).http.get('*/api/v1/databases/shop/tables', () =>
          await_json()({
            items: [
              {
                schema: 'public',
                name: 'mystery',
                classes: ['plain'],
                distribution: null,
                ledger: null,
                homeShardGroupId: 0,
                estimatedRows: 1,
                sizeBytes: 1,
                storage: null,
              },
            ],
            nextCursor: null,
          }),
        ),
      ],
    )
    await renderApp(db)
    const row = (await screen.findByRole('link', { name: 'public.mystery' })).closest('tr')!
    expect(within(row).getByTestId('storage-cell')).toHaveAttribute('data-storage', 'unknown')
    expect(within(row).getByText('Not available')).toBeInTheDocument()
  })

  it('a partitioned table is converted partition by partition: the list says so', async () => {
    sw('analytics=timeseries')
    await renderApp(db)
    const button = await screen.findByTestId('convert-metrics')
    expect(button).toHaveAttribute('aria-disabled', 'true')
    expect(button).toHaveTextContent(/one partition at a time/i)
  })

  it('Convert storage... on a table opens the dialog of that table', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    await renderApp(db)
    await user.click(await screen.findByTestId('convert-clicks'))
    expect(await dialogOf(/^convert storage of table clicks/i)).toBeTruthy()
  })
})

const await_json =
  () =>
  (body: unknown, status = 200) =>
    new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': status >= 400 ? 'application/problem+json' : 'application/json' },
    })

describe('the table page: storage and partitions', () => {
  it('says the format of the table and of each partition, with the ratio and the rows', async () => {
    sw('analytics=timeseries')
    await renderApp(table('metrics'))
    const list = within(await screen.findByTestId('partition-list'))
    expect(screen.getByTestId('table-storage')).toHaveTextContent('Mixed (2 columnar of 10)')
    expect(screen.getByTestId('table-storage-detail')).toHaveTextContent(/stripes/)
    const rows = list.getAllByRole('row').slice(1)
    expect(rows).toHaveLength(10)
    const columnar = rows.filter((r) => within(r).queryByText('Columnar'))
    expect(columnar).toHaveLength(2)
    for (const r of columnar) expect(r).toHaveTextContent('6.2 x')
    const heap = rows.filter((r) => within(r).queryByText('Row'))
    expect(heap).toHaveLength(8)
    expect(within(heap[0]!).getAllByTestId('na').length).toBeGreaterThan(0)
  })

  it('filters the partitions by format', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    await renderApp(table('metrics'))
    const list = within(await screen.findByTestId('partition-list'))
    await user.selectOptions(screen.getByTestId('partition-format'), 'columnar')
    expect(list.getAllByRole('row').slice(1)).toHaveLength(2)
    await user.selectOptions(screen.getByTestId('partition-format'), 'heap')
    expect(list.getAllByRole('row').slice(1)).toHaveLength(8)
  })

  it('a plain partitioned table has no create, attach, detach or drop, but converts a partition', async () => {
    sw('analytics=timeseries')
    await renderApp(table('metrics'))
    await screen.findByTestId('partition-list')
    expect(screen.queryByTestId('create-partition')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^detach$/i })).not.toBeInTheDocument()
    expect(screen.getAllByRole('button', { name: /^convert storage/i })).toHaveLength(11)
  })

  it('converts a partition: the dialog warns, sends the codec and the sort order, and the row becomes columnar', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    mockConfig.taskDurationMs = 80
    recordBodies('PUT', '/storage')
    await renderApp(table('metrics'))
    const list = within(await screen.findByTestId('partition-list'))
    const heap = list
      .getAllByRole('row')
      .slice(1)
      .find((r) => within(r).queryByText('Row'))!
    const name = within(heap).getAllByRole('rowheader')[0]!.textContent!.trim()
    await user.click(within(heap).getByRole('button', { name: /^convert storage$/i }))
    const dialog = await dialogOf(new RegExp(`^convert storage of partition ${name}`, 'i'))
    expect(dialog.getByTestId('storage-partition-note')).toHaveTextContent(
      /writers of this partition wait/i,
    )
    await user.selectOptions(dialog.getByLabelText('Compression'), 'lz4')
    await user.type(dialog.getByLabelText(/sort rows by/i), 'ts, device')
    await user.click(dialog.getByRole('button', { name: /^convert$/i }))
    expect(await dialog.findByText('Finished.')).toBeInTheDocument()
    expect(bodies).toEqual([
      { accessMethod: 'columnar', compression: 'lz4', sortBy: ['ts', 'device'] },
    ])
    await user.click(dialog.getByRole('button', { name: /^close$/i }))
    await waitFor(() =>
      expect(
        within(screen.getByTestId(`partition-${name}`)).getByText('Columnar'),
      ).toBeInTheDocument(),
    )
  })

  it('a conversion in progress marks the row of its partition with the progress', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    mockConfig.taskDurationMs = 60_000
    await renderApp(table('metrics'))
    const list = within(await screen.findByTestId('partition-list'))
    const heap = list
      .getAllByRole('row')
      .slice(1)
      .find((r) => within(r).queryByText('Row'))!
    const name = within(heap).getAllByRole('rowheader')[0]!.textContent!.trim()
    await user.click(within(heap).getByRole('button', { name: /^convert storage$/i }))
    const dialog = await dialogOf(/^convert storage of partition/i)
    await user.click(dialog.getByRole('button', { name: /^convert$/i }))
    await dialog.findByTestId('cancel-task')
    await user.click(dialog.getByRole('button', { name: /hide \(keeps running\)/i }))
    expect(
      await screen.findByTestId(`converting-${name}`, {}, { timeout: 30_000 }),
    ).toHaveTextContent(/converting/i)
    // While it runs, the other buttons of the table say why they are off.
    expect(screen.getByTestId('policy-edit')).toHaveAttribute('aria-disabled', 'true')
  })

  it('a refusal of the server is listed reason by reason and nothing starts', async () => {
    sw('analytics=timeseries,publication')
    const user = userEvent.setup()
    await renderApp(table('clicks'))
    await user.click(await screen.findByTestId('convert-storage'))
    const dialog = await dialogOf(/^convert storage of table clicks/i)
    expect(dialog.getByTestId('storage-table-note')).toHaveTextContent(
      /blocks readers and writers/i,
    )
    // The server's own check (dryRun) comes before the start: its blockers are listed and Convert stays off.
    const blockers = await dialog.findByTestId('table-blockers')
    expect(blockers).toHaveTextContent(/a publication/i)
    expect(blockers).toHaveTextContent(/is in a publication/i)
    expect(dialog.getByRole('button', { name: /^convert$/i })).toBeDisabled()
    expect(getState().tasks.some((t) => t.task.kind === 'table.setStorage')).toBe(false)
  })

  it('when the server cannot preview, the start is asked and the refusal comes back with its typed blockers', async () => {
    sw('analytics=timeseries,publication')
    mockServer.use(
      (await import('msw')).http.put('*/storage', ({ request }) =>
        new URL(request.url).searchParams.get('dryRun') === 'true'
          ? await_json()(
              { type: 'about:blank', title: 'x', status: 422, code: 'unsupported_operation' },
              422,
            )
          : await_json()(
              {
                type: 'about:blank',
                title: 'x',
                status: 409,
                code: 'conversion_refused',
                detail: 'is in a publication',
                blockers: [
                  { code: 'publication', object: 'public.clicks', message: 'is in a publication' },
                ],
              },
              409,
            ),
      ),
    )
    const user = userEvent.setup()
    await renderApp(table('clicks'))
    await user.click(await screen.findByTestId('convert-storage'))
    const dialog = await dialogOf(/^convert storage of table clicks/i)
    const start = dialog.getByRole('button', { name: /^convert$/i })
    await enabled(start)
    await user.click(start)
    const refusal = await dialog.findByTestId('storage-refusal')
    expect(refusal).toHaveTextContent('The storage format cannot be changed')
    expect(within(refusal).getByTestId('table-blockers')).toHaveTextContent(/is in a publication/i)
  })

  it('Convert stays off while the server checks the change', async () => {
    sw('analytics=timeseries')
    const { http, HttpResponse, delay } = await import('msw')
    mockServer.use(
      http.put('*/storage', async ({ request }) => {
        if (new URL(request.url).searchParams.get('dryRun') !== 'true')
          return HttpResponse.json({}, { status: 500 })
        await delay(400)
        return HttpResponse.json({
          current: 'heap',
          target: 'columnar',
          willConvert: true,
          blockers: [],
          notes: [],
        })
      }),
    )
    const user = userEvent.setup()
    await renderApp(table('clicks'))
    await user.click(await screen.findByTestId('convert-storage'))
    const dialog = await dialogOf(/^convert storage of table clicks/i)
    const start = dialog.getByRole('button', { name: /^convert$/i })
    expect(await dialog.findByTestId('storage-preview-loading')).toBeInTheDocument()
    expect(start).toBeDisabled()
    await enabled(start)
    expect(dialog.queryByTestId('storage-preview-loading')).not.toBeInTheDocument()
  })

  it('the preview of a change that can be made lists its notes and lets it start', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    await renderApp(table('clicks'))
    await user.click(await screen.findByTestId('convert-storage'))
    const dialog = await dialogOf(/^convert storage of table clicks/i)
    expect(await dialog.findByTestId('storage-notes')).toHaveTextContent(/ACCESS EXCLUSIVE/)
    await enabled(dialog.getByRole('button', { name: /^convert$/i }))
    expect(dialog.queryByTestId('table-blockers')).not.toBeInTheDocument()
  })

  it('an index or a trigger fails the task at the convert step, in words, and the dialog says it is found then', async () => {
    const user = userEvent.setup()
    mockConfig.taskDurationMs = 80
    await renderApp('/databases/reports/tables/public/exports')
    await user.click(await screen.findByTestId('convert-storage'))
    const dialog = await dialogOf(/^convert storage of table exports/i)
    expect(dialog.getByTestId('storage-late-note')).toHaveTextContent(/found when the task runs/i)
    const start = dialog.getByRole('button', { name: /^convert$/i })
    await enabled(start)
    expect(dialog.queryByTestId('table-blockers')).not.toBeInTheDocument()
    await user.click(start)
    expect(
      await dialog.findByText(
        /has an index \(exports_pkey\).*has a trigger \(exports_audit\)/i,
        {},
        { timeout: 30_000 },
      ),
    ).toBeInTheDocument()
  })

  it('a table that is columnar already can be converted again: it says it sets the options', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    mockConfig.taskDurationMs = 60
    await signedIn()
    const first = await endpoints.setTableStorage('shop', 'public', 'clicks', {
      accessMethod: 'columnar',
    })
    await waitFor(async () => expect((await endpoints.getTask(first.id)).state).toBe('done'))
    await renderApp(table('clicks'))
    await user.click(await screen.findByTestId('convert-storage'))
    const dialog = await dialogOf(/^convert storage of table clicks/i)
    await user.click(dialog.getByTestId('storage-columnar'))
    expect(await dialog.findByTestId('storage-options-only')).toHaveTextContent(
      /options for the stripes/i,
    )
    await enabled(dialog.getByRole('button', { name: /^convert$/i }))
    expect(dialog.queryByTestId('storage-same')).not.toBeInTheDocument()
  })

  it('what the console knows stops the start before it asks: a ledger table', async () => {
    const user = userEvent.setup()
    sw('analytics=timeseries')
    getState()
      .databases.find((d) => d.info.name === 'shop')!
      .tables.find((t) => t.name === 'clicks')!.classes = ['ledger']
    await renderApp(table('clicks'))
    await user.click(await screen.findByTestId('convert-storage'))
    const dialog = await dialogOf(/^convert storage of table clicks/i)
    expect(dialog.getByTestId('storage-blocker')).toHaveTextContent(/ledger table/i)
    expect(dialog.getByRole('button', { name: /^convert$/i })).toBeDisabled()
  })

  it('a table already in the format chosen says so; the options of the codec are checked', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    await renderApp(table('clicks'))
    await user.click(await screen.findByTestId('convert-storage'))
    const dialog = await dialogOf(/^convert storage of table clicks/i)
    await user.click(dialog.getByTestId('storage-heap'))
    expect(dialog.getByTestId('storage-same')).toBeInTheDocument()
    expect(dialog.getByRole('button', { name: /^convert$/i })).toBeDisabled()
    await user.click(dialog.getByTestId('storage-columnar'))
    await clear(user, dialog.getByTestId('storage-level'), '25')
    await user.click(dialog.getByRole('button', { name: /^convert$/i }))
    expect((await dialog.findAllByText(/use a whole number from 1 to 19/i)).length).toBeGreaterThan(
      0,
    )
    expect(getState().tasks.some((t) => t.task.kind === 'table.setStorage')).toBe(false)
    await user.click(dialog.getByText('Advanced'))
    await user.type(dialog.getByTestId('storage-stripe'), '10')
    await user.click(dialog.getByRole('button', { name: /^convert$/i }))
    expect(
      await dialog.findByText(/rows per stripe: use a whole number of 1000 or more/i),
    ).toBeInTheDocument()
  })

  it('a cancelled conversion says the table is as it was', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    mockConfig.taskDurationMs = 60_000
    await renderApp(table('clicks'))
    await user.click(await screen.findByTestId('convert-storage'))
    const dialog = await dialogOf(/^convert storage of table clicks/i)
    await user.click(dialog.getByRole('button', { name: /^convert$/i }))
    await user.click(await dialog.findByTestId('cancel-task'))
    expect(await dialog.findByTestId('task-cancelled')).toHaveTextContent(/the table is as it was/i)
  })

  it('a missing extension is said with its own words, a failed copy with the task error', async () => {
    sw('analytics=timeseries,columnar:missing')
    const user = userEvent.setup()
    await renderApp(table('clicks'))
    await user.click(await screen.findByTestId('convert-storage'))
    const dialog = await dialogOf(/^convert storage of table clicks/i)
    await user.click(dialog.getByRole('button', { name: /^convert$/i }))
    expect(await dialog.findByText('An extension is missing')).toBeInTheDocument()
    expect(dialog.getByText(/kingres_columnar is not installed/i)).toBeInTheDocument()
  })

  it.each([
    ['viewer', /kingres_admin/],
    ['ops', /kingres_admin/],
  ])('%s sees the storage buttons disabled with the reason', async (user, reason) => {
    sw('analytics=timeseries')
    await renderApp(table('metrics'), { user })
    await screen.findByTestId('partition-list')
    const buttons = screen.getAllByRole('button', { name: /^convert storage/i })
    for (const b of buttons) await waitFor(() => expect(b).toHaveAttribute('aria-disabled', 'true'))
    expect(buttons[0]).toHaveTextContent(reason)
  })

  it('has no accessibility violations (the table, the partitions, the policy)', async () => {
    sw('analytics=timeseries')
    const { container } = await renderApp(table('metrics'))
    await screen.findByTestId('policy-runs')
    expect(await axe(container)).toHaveNoViolations()
  })
})

describe('the policy panel and its runs', () => {
  it('shows the policy, the health line and the last run', async () => {
    sw('analytics=timeseries')
    await renderApp(table('metrics'))
    const facts = within(await screen.findByTestId('policy-facts'))
    expect(facts.getByTestId('policy-ahead-text')).toHaveTextContent(
      /2 days \d+ (hours?|minutes?) ahead, below the 7 days the policy keeps/i,
    )
    expect(facts.getByTestId('policy-health')).toHaveAttribute('data-ok', 'false')
    expect(facts.getByTestId('policy-problems')).toHaveTextContent(
      /fewer partitions ahead than the policy keeps/i,
    )
    expect(facts.getByText('1 day')).toBeInTheDocument()
    expect(facts.getByText('90 days (drop)')).toBeInTheDocument()
    expect(facts.getByText('4 days')).toBeInTheDocument()
    expect(facts.getByTestId('policy-last-run')).toHaveTextContent(/done/i)
  })

  it('lists the runs of the scheduler and opens one with its actions, the failure in words', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    await renderApp(table('metrics'))
    const runs = within(await screen.findByTestId('policy-runs'))
    const rows = runs.getAllByRole('row').slice(1)
    expect(rows).toHaveLength(3)
    expect(rows[1]).toHaveTextContent(/run now/i)
    expect(rows[1]).toHaveTextContent(/partly done/i)
    expect(rows[0]).toHaveTextContent(/scheduled/i)
    expect(rows[0]).toHaveTextContent(/system/)
    const partial = Number(rows[1]!.getAttribute('data-testid')!.replace('run-', ''))
    await user.click(screen.getByTestId(`run-open-${partial}`))
    const dialog = await dialogOf(new RegExp(`^run ${partial} of public\\.metrics`, 'i'))
    expect(await dialog.findByTestId('run-error')).toHaveTextContent(
      /1 of 2 actions did not finish/i,
    )
    const actions = dialog.getByTestId('partition-actions')
    expect(actions).toHaveTextContent(
      /waited for the lock on `metrics` and gave up; will retry at the next run/i,
    )
    expect(actions).toHaveTextContent('Convert to columnar')
    expect(await axe(screen.getByRole('dialog'))).toHaveNoViolations()
  })

  it('a table without a policy says so and offers to set one', async () => {
    sw('analytics=timeseries')
    await signedIn()
    await endpoints.deletePartitionPolicy('shop', 'public', 'metrics')
    await renderApp(table('metrics'))
    expect(await screen.findByTestId('policy-none')).toBeInTheDocument()
    expect(screen.getByTestId('policy-edit')).toHaveTextContent('Set a policy')
    expect(screen.queryByTestId('policy-run')).not.toBeInTheDocument()
    expect(screen.queryByTestId('policy-runs')).not.toBeInTheDocument()
  })

  it('a table partitioned on a column that is not a time says it needs one', async () => {
    sw('tables=partitions')
    getState()
      .databases.find((d) => d.info.name === 'shop')!
      .tables.find((t) => t.name === 'measurements')!.partitioning = {
      method: 'range',
      key: 'sensor_id',
      partitionCount: 3,
    }
    await renderApp(table('measurements'))
    expect(await screen.findByTestId('policy-no-time-key')).toBeInTheDocument()
  })

  it('Run now lists what the run would do, warns about the data it deletes, and starts the task', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    mockConfig.taskDurationMs = 80
    await signedIn()
    await endpoints.putPartitionPolicy('shop', 'public', 'metrics', {
      partitionEvery: 'P1D',
      retention: 'P5D',
      retentionAction: 'drop',
    })
    await renderApp(table('metrics'))
    await user.click(await screen.findByTestId('policy-run'))
    const dialog = await dialogOf(/^run the policy of public\.metrics now/i)
    expect(await dialog.findByTestId('run-plan-deletes')).toHaveTextContent(
      /dropping \d+ partitions deletes their rows\. the size of the data is not known/i,
    )
    expect(dialog.getByTestId('partition-actions')).toHaveTextContent('Drop')
    await user.click(dialog.getByRole('button', { name: /^run now$/i }))
    expect(await dialog.findByText('Finished.')).toBeInTheDocument()
    expect(
      getState().tasks.some((t) => t.task.kind === 'partition.maintain' && t.task.state === 'done'),
    ).toBe(true)
    await user.click(dialog.getByRole('button', { name: /^close$/i }))
    await waitFor(() =>
      expect(within(screen.getByTestId('policy-runs')).getAllByRole('row')).toHaveLength(5),
    )
  })

  it('a run that is already going is refused in words', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    mockConfig.taskDurationMs = 60_000
    await signedIn()
    await endpoints.runPartitionPolicy('shop', 'public', 'metrics')
    await renderApp(table('metrics'))
    // The panel holds its own buttons while a run goes on.
    const run = await screen.findByTestId('policy-run')
    await waitFor(() => expect(run).toHaveAttribute('aria-disabled', 'true'))
    expect(run).toHaveTextContent(/a change of this table is running/i)
    void user
  })

  it('Remove policy says the partitions stay, and removes only the policy', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    await renderApp(table('metrics'))
    const before = (await endpoints.listTablePartitions('shop', 'public', 'metrics')).items.length
    await user.click(await screen.findByTestId('policy-remove'))
    const dialog = await dialogOf(/^remove the policy of public\.metrics/i)
    expect(dialog.getByTestId('remove-policy-text')).toHaveTextContent(
      /the partitions stay as they are/i,
    )
    await user.click(dialog.getByRole('button', { name: /^remove policy$/i }))
    expect(await screen.findByTestId('policy-none')).toBeInTheDocument()
    expect((await endpoints.listTablePartitions('shop', 'public', 'metrics')).items.length).toBe(
      before,
    )
  })

  it.each([
    ['viewer', /kingres_admin/],
    ['ops', /kingres_admin/],
  ])(
    '%s reads the policy and the runs; Edit, Run now and Remove are off with the reason',
    async (user, reason) => {
      sw('analytics=timeseries')
      await renderApp(table('metrics'), { user })
      await screen.findByTestId('policy-facts')
      for (const id of ['policy-edit', 'policy-run', 'policy-remove']) {
        const b = screen.getByTestId(id)
        await waitFor(() => expect(b).toHaveAttribute('aria-disabled', 'true'))
        expect(b).toHaveTextContent(reason)
      }
      expect(await screen.findByTestId('policy-runs')).toBeInTheDocument()
    },
  )

  it('a server that does not know the operations shows calm information', async () => {
    sw('analytics=timeseries,old')
    await renderApp(table('metrics'))
    await waitFor(() =>
      expect(
        within(screen.getByTestId('policy-panel')).getAllByText(
          /not (yet )?available|not implemented|not supported/i,
        ).length,
      ).toBeGreaterThan(0),
    )
  })
})

describe('the time-partitioning policy dialog', () => {
  async function open(user: User, name = 'metrics', from = table(name)) {
    await renderApp(from)
    await user.click(await screen.findByTestId('policy-edit'))
    return dialogOf(/^time partitioning of public\./i)
  }

  it('starts from the stored policy and previews what the next run would do, with sizes, without changing anything', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    const dialog = await open(user)
    await dialog.findByTestId('policy-form')
    expect(dialog.getByTestId('policy-retention')).toHaveValue(90)
    expect(dialog.getByTestId('policy-timezone')).toHaveValue('UTC')
    await user.click(dialog.getByTestId('policy-preview'))
    const summary = await dialog.findByTestId('policy-preview-summary')
    expect(summary).toHaveTextContent(/create 5 partitions, convert 1, drop 0 and detach 0/i)
    expect(dialog.getByTestId('partition-actions')).toHaveTextContent('Create')
    expect(getState().analytics!.runs['shop/public.metrics']).toHaveLength(3)
  })

  it('a shorter retention shows the data it would delete, needs a preview of the form as it is, and the name typed', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    recordBodies('PUT', '/partition-policy')
    const dialog = await open(user)
    await dialog.findByTestId('policy-form')
    const save = dialog.getByTestId('policy-save')
    await enabled(save)
    await clear(user, dialog.getByTestId('policy-retention'), '5')
    expect(await dialog.findByTestId('policy-need-preview')).toBeInTheDocument()
    expect(save).toBeDisabled()
    await user.click(dialog.getByTestId('policy-preview'))
    const deletes = await dialog.findByTestId('policy-preview-deletes')
    expect(deletes).toHaveTextContent(
      /dropping \d+ partitions deletes their rows\. the size of the data is not known\. saving the policy deletes nothing now; the next run does/i,
    )
    expect(save).toBeDisabled()
    await user.type(dialog.getByLabelText(/type .*metrics.* to confirm/i), 'metrics')
    await enabled(save)
    // Changing the form makes the preview stale and the save off again.
    await clear(user, dialog.getByTestId('policy-retention'), '6')
    expect(await dialog.findByTestId('policy-preview-stale')).toBeInTheDocument()
    expect(save).toBeDisabled()
    await user.click(dialog.getByTestId('policy-preview'))
    await waitFor(() =>
      expect(dialog.queryByTestId('policy-preview-stale')).not.toBeInTheDocument(),
    )
    await enabled(save)
    await user.click(save)
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(bodies).toHaveLength(1)
    expect(bodies[0]).toMatchObject({
      partitionEvery: 'P1D',
      retention: 'P6D',
      retentionAction: 'drop',
    })
    expect(await screen.findByText('6 days (drop)')).toBeInTheDocument()
  })

  it('a policy that keeps the retention or lengthens it saves without typing the name', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    const dialog = await open(user)
    await dialog.findByTestId('policy-form')
    await clear(user, dialog.getByTestId('policy-ahead'), '9')
    expect(dialog.queryByLabelText(/to confirm/i)).not.toBeInTheDocument()
    const save = dialog.getByTestId('policy-save')
    await enabled(save)
    await user.click(save)
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(await screen.findByText('9 days')).toBeInTheDocument()
  })

  it('a table with no policy starts from the defaults, retention off, and saves a first policy', async () => {
    sw('analytics=timeseries')
    await signedIn()
    await endpoints.deletePartitionPolicy('shop', 'public', 'metrics')
    const user = userEvent.setup()
    recordBodies('PUT', '/partition-policy')
    const dialog = await open(user)
    await dialog.findByTestId('policy-form')
    expect(dialog.getByTestId('policy-retention-on')).not.toBeChecked()
    expect(dialog.getByTestId('policy-columnar-on')).not.toBeChecked()
    await user.click(dialog.getByTestId('policy-columnar-on'))
    await clear(user, dialog.getByTestId('policy-columnar'), '30')
    const save = dialog.getByTestId('policy-save')
    await enabled(save)
    await user.click(save)
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(bodies[0]).toMatchObject({
      partitionEvery: 'P1D',
      createAhead: 'P7D',
      retention: null,
      columnarAfter: 'P30D',
      compression: 'zstd',
      compressionLevel: 3,
      timezone: 'UTC',
      runEvery: 'PT1H',
      enabled: true,
    })
    expect(await screen.findByTestId('policy-facts')).toBeInTheDocument()
  })

  it('shows what the server refused, field by field, next to the field', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    const dialog = await open(user)
    await dialog.findByTestId('policy-form')
    await clear(user, dialog.getByTestId('policy-timezone'), 'Nowhere/Land')
    await clear(user, dialog.getByTestId('policy-retention'), '2')
    await clear(user, dialog.getByTestId('policy-columnar'), '10')
    await user.click(dialog.getByTestId('policy-preview'))
    expect(
      await dialog.findByText(/not a time zone name known to the server/i, { selector: '.error' }),
    ).toBeInTheDocument()
    expect(
      await dialog.findByText(/convert before the retention drops the partition/i, {
        selector: '.error',
      }),
    ).toBeInTheDocument()
  })

  it('checks the numbers before it asks', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    const dialog = await open(user)
    await dialog.findByTestId('policy-form')
    await clear(user, dialog.getByTestId('policy-ahead'), '0')
    expect((await dialog.findAllByText(/whole number of 1 or more/i)).length).toBeGreaterThan(0)
    expect(dialog.getByTestId('policy-preview')).toBeDisabled()
    expect(dialog.getByTestId('policy-save')).toBeDisabled()
  })

  it('a custom interval is hours that divide a day, or days', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    const dialog = await open(user)
    await dialog.findByTestId('policy-form')
    await user.selectOptions(dialog.getByTestId('policy-interval'), 'custom')
    expect(dialog.getByTestId('policy-custom-count').tagName).toBe('SELECT')
    await user.selectOptions(dialog.getByTestId('policy-custom-unit'), 'day')
    expect(dialog.getByTestId('policy-custom-count').tagName).toBe('INPUT')
  })

  it('a table that is not partitioned by range on a time column gets the explanation and the statement, and no form', async () => {
    const user = userEvent.setup()
    await renderApp(db)
    await user.click(await screen.findByTestId('policy-notes'))
    const dialog = await dialogOf(/^time partitioning of public\.notes/i)
    expect(dialog.getByTestId('policy-not-partitioned')).toHaveTextContent(
      /already partitioned by range on a date or timestamp column/i,
    )
    expect(dialog.getByTestId('policy-example')).toHaveTextContent(/PARTITION BY RANGE/)
    expect(dialog.queryByTestId('policy-form')).not.toBeInTheDocument()
    expect(dialog.queryByTestId('policy-save')).not.toBeInTheDocument()
  })

  it('a table with a default partition shows the refusal and the way out', async () => {
    sw('analytics=timeseries')
    await signedIn()
    const list = await endpoints.listTablePartitions('shop', 'public', 'metrics')
    getState().partitions!['shop/public.metrics'] = [
      ...list.items,
      {
        ...list.items[0]!,
        name: 'metrics_default',
        bound: 'DEFAULT',
        isDefault: true,
        rangeFrom: null,
        rangeTo: null,
      },
    ]
    const user = userEvent.setup()
    const dialog = await open(user)
    expect(await dialog.findByTestId('policy-default-partition')).toHaveTextContent(
      /drop the default partition/i,
    )
    expect(dialog.queryByTestId('policy-save')).not.toBeInTheDocument()
  })

  it('a missing extension is said when the policy is saved', async () => {
    sw('analytics=timeseries,timepart:missing')
    const user = userEvent.setup()
    const dialog = await open(user)
    await dialog.findByTestId('policy-form')
    const save = dialog.getByTestId('policy-save')
    await enabled(save)
    await user.click(save)
    expect(await dialog.findByText('An extension is missing')).toBeInTheDocument()
    expect(dialog.getByText(/kingres_timepart is not installed/i)).toBeInTheDocument()
  })

  it('has no accessibility violations', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    const dialog = await open(user)
    await dialog.findByTestId('policy-form')
    await user.click(dialog.getByTestId('policy-retention-on'))
    await user.click(dialog.getByTestId('policy-columnar-on'))
    await user.click(dialog.getByTestId('policy-preview'))
    await dialog.findByTestId('policy-preview-summary')
    expect(await axe(screen.getByRole('dialog'))).toHaveNoViolations()
  })
})

describe('the alerts of the scheduler', () => {
  it('partitionsAheadLow links to the partitions panel of its table', async () => {
    sw('analytics=ahead-low')
    const user = userEvent.setup()
    await renderApp('/alerts')
    const item = (await screen.findAllByText('Partitions ahead are running low'))[0]!
    const row = item.closest('li, tr, article, section') as HTMLElement
    const link = await within(row).findByRole('link', { name: 'public.metrics' })
    await user.click(link)
    expect(await screen.findByTestId('policy-panel')).toBeInTheDocument()
  })

  it('an alert with no table in its subject links to nothing and shows the qualifier as sent', async () => {
    sw('analytics=timeseries')
    getState().alerts.unshift({
      id: 999,
      kind: 'partitionPolicyFailed',
      severity: 'warning',
      state: 'open',
      title: 'A partitioning policy failed',
      message: 'x',
      serverId: null,
      databaseName: 'shop',
      raisedAt: new Date().toISOString(),
      subject: {
        serverId: null,
        behindServerId: null,
        shardGroupId: null,
        databaseName: 'shop',
        aspect: 'something-new',
      },
    })
    await renderApp('/alerts')
    expect(await screen.findByText(/something-new/)).toBeInTheDocument()
    expect(screen.queryByTestId('alert-policy-table')).not.toBeInTheDocument()
  })
})

describe('the plan summary of the workbench', () => {
  async function explain(user: User, sql: string) {
    await renderApp('/workbench')
    await screen.findByRole('option', { name: 'shop' })
    await clear(user, screen.getByLabelText(/^SQL/), sql)
    await user.click(screen.getByRole('button', { name: /^run$/i }))
    return screen.findByTestId('plan')
  }

  it('puts the path above the text of a repartitioned join', async () => {
    const user = userEvent.setup()
    const plan = within(
      await explain(
        user,
        'explain select * from orders o join events e on o.customer_id = e.customer_id',
      ),
    )
    expect(plan.getByTestId('plan-summary')).toHaveTextContent(
      /path: repartitioned join \(3 of 3 groups\)/i,
    )
    expect(plan.getByTestId('plan-text')).toHaveTextContent(/Repartition: events by customer_id/)
  })

  it('says a colocated join, a partial aggregate, and why a path was not used', async () => {
    const user = userEvent.setup()
    expect(
      within(
        await explain(
          user,
          'explain select * from orders o join customers c on o.customer_id = c.customer_id',
        ),
      ).getByTestId('plan-summary'),
    ).toHaveTextContent(/path: colocated join/i)
  })

  it('says a partial aggregate', async () => {
    const user = userEvent.setup()
    expect(
      within(
        await explain(user, 'explain select region, sum(total) from orders group by region'),
      ).getByTestId('plan-summary'),
    ).toHaveTextContent(/path: partial aggregate/i)
  })

  it('says why a path was not used, from the Pushdown line', async () => {
    const user = userEvent.setup()
    const plan = within(await explain(user, 'explain (verbose) select * from orders'))
    expect(plan.getByTestId('plan-summary')).toHaveTextContent(/path: scan/i)
    expect(plan.getByTestId('plan-not-used')).toHaveTextContent(
      /not used: the statement has no join or aggregate to push/i,
    )
  })

  it('says the rewrite of count(DISTINCT) when the database has the approximate mode on, and lists the sketch functions', async () => {
    sw('analytics=on')
    const user = userEvent.setup()
    const plan = within(
      await explain(user, 'explain select count(distinct customer_id) from orders'),
    )
    expect(plan.getByTestId('plan-rewritten')).toHaveTextContent(
      /rewritten: count\(DISTINCT\) -> approximate/i,
    )
    expect(screen.getByTestId('sketch-help')).toHaveTextContent(
      /hll_add_agg.*topn_add_agg.*tdigest_quantile/,
    )
  })

  it('shows no summary for a plan with no line it knows, and no help text when the database has no sketches', async () => {
    mockServer.use(
      (await import('msw')).http.post('*/api/v1/query', () =>
        await_json()({
          columns: [{ name: 'QUERY PLAN', type: 'text' }],
          rows: [['Seq Scan on t  (cost=0.00..1.00 rows=1 width=4)']],
          rowCount: 1,
          truncated: false,
          commandTag: 'EXPLAIN',
          durationMs: 1,
        }),
      ),
    )
    const user = userEvent.setup()
    const plan = within(await explain(user, 'explain select 1'))
    expect(plan.getByTestId('plan-text')).toHaveTextContent('Seq Scan on t')
    expect(plan.queryByTestId('plan-summary')).not.toBeInTheDocument()
    expect(screen.queryByTestId('sketch-help')).not.toBeInTheDocument()
  })

  it('a result that is not an EXPLAIN stays a table, and the note says EXPLAIN ANALYZE and MERGE run the statement', async () => {
    const user = userEvent.setup()
    await renderApp('/workbench')
    await screen.findByRole('option', { name: 'shop' })
    expect(screen.getByTestId('mode')).toHaveTextContent(
      /explain analyze and\s+merge run the statement too/i,
    )
    await user.click(screen.getByRole('button', { name: /^run$/i }))
    expect(await screen.findByRole('table', { name: /query result/i })).toBeInTheDocument()
    expect(screen.queryByTestId('plan')).not.toBeInTheDocument()
  })

  it('has no accessibility violations', async () => {
    const user = userEvent.setup()
    await explain(
      user,
      'explain select * from orders o join events e on o.customer_id = e.customer_id',
    )
    expect(await axe(document.body)).toHaveNoViolations()
  })
})

describe('the owners of the tables in the console (contract 0.18.0)', () => {
  it('the setting is off by default; turning it on says what it means and asks first, and sends only that group', async () => {
    const user = userEvent.setup()
    recordBodies('PATCH', '/settings/cluster')
    await renderApp('/settings')
    const box = await screen.findByTestId('allow-non-superuser-owners')
    expect(box).not.toBeChecked()
    expect(screen.getByTestId('allow-non-superuser-owners-text')).toHaveTextContent(
      /run the code of the owner of the table.*as the\s+service role/i,
    )
    await user.click(box)
    await user.click(screen.getByRole('button', { name: /^save settings/i }))
    const dialog = await dialogOf(/run the code of table owners who are not superusers\?/i)
    expect(dialog.getByTestId('owners-confirm-text')).toHaveTextContent(/trust every owner/i)
    expect(bodies).toEqual([])
    await user.click(dialog.getByRole('button', { name: /^turn on$/i }))
    expect(await screen.findByText('Settings saved.')).toBeInTheDocument()
    expect(bodies).toEqual([{ analytics: { allowNonSuperuserOwners: true } }])
    expect(getState().settings.analytics?.allowNonSuperuserOwners).toBe(true)
  })

  it('turning it off needs no confirmation', async () => {
    getState().settings.analytics = { allowNonSuperuserOwners: true }
    const user = userEvent.setup()
    recordBodies('PATCH', '/settings/cluster')
    await renderApp('/settings')
    const box = await screen.findByTestId('allow-non-superuser-owners')
    expect(box).toBeChecked()
    await user.click(box)
    await user.click(screen.getByRole('button', { name: /^save settings/i }))
    expect(await screen.findByText('Settings saved.')).toBeInTheDocument()
    expect(bodies).toEqual([{ analytics: { allowNonSuperuserOwners: false } }])
  })

  it('the storage dialog names the owner, explains the blocker and links to the setting; Convert stays off', async () => {
    sw('analytics=timeseries,owner:untrusted')
    const user = userEvent.setup()
    await renderApp(table('clicks'))
    await user.click(await screen.findByTestId('convert-storage'))
    const dialog = await dialogOf(/^convert storage of table clicks/i)
    const notice = await dialog.findByTestId('owner-not-trusted')
    expect(notice).toHaveTextContent(/owned by analyst, who is not a superuser/i)
    expect(within(notice).getByRole('link', { name: /cluster settings/i })).toHaveAttribute(
      'href',
      '/settings',
    )
    expect(dialog.getByTestId('table-blockers')).toHaveTextContent(
      /the owner of the table is not trusted/i,
    )
    expect(dialog.getByRole('button', { name: /^convert$/i })).toBeDisabled()
  })

  it('the policy panel shows the owner and why runs are refused; the setting clears it', async () => {
    sw('analytics=timeseries,owner:untrusted')
    const user = userEvent.setup()
    await renderApp(table('metrics'))
    expect(await screen.findByTestId('owner-not-trusted')).toHaveTextContent(
      /scheduled run raises one alert/i,
    )
    expect(screen.getByTestId('policy-owner')).toHaveTextContent('analyst (not trusted)')
    // Run now is refused by the server in words.
    await user.click(await screen.findByTestId('policy-run'))
    const dialog = await dialogOf(/^run the policy of public\.metrics now/i)
    await user.click(await dialog.findByRole('button', { name: /^run now$/i }))
    expect(
      await dialog.findByText(/this table cannot have a partitioning policy/i),
    ).toBeInTheDocument()
    expect(dialog.getByText(/analytics\.allowNonSuperuserOwners/)).toBeInTheDocument()
    await user.click(dialog.getByRole('button', { name: /^cancel$/i }))
    await endpoints.updateClusterSettings({ analytics: { allowNonSuperuserOwners: true } })
    await waitFor(() => expect(screen.queryByTestId('owner-not-trusted')).not.toBeInTheDocument(), {
      timeout: 15_000,
    })
    expect(screen.getByTestId('policy-owner')).toHaveTextContent('analyst (trusted)')
  })

  it('a trusted owner shows no notice', async () => {
    sw('analytics=timeseries')
    await renderApp(table('metrics'))
    expect(await screen.findByTestId('policy-owner')).toHaveTextContent('postgres (trusted)')
    expect(screen.queryByTestId('owner-not-trusted')).not.toBeInTheDocument()
  })
})

describe('the fixes of the independent review (P12W1 to P12W10)', () => {
  const { http, HttpResponse } = {} as never as { http: never; HttpResponse: never }
  void http
  void HttpResponse
  const msw = () => import('msw')

  it('P12W1: a plan without sizes says the size is not known, names the partitions, and never says 0 B', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    mockConfig.taskDurationMs = 80
    await signedIn()
    await endpoints.putPartitionPolicy('shop', 'public', 'metrics', {
      partitionEvery: 'P1D',
      retention: 'P5D',
      retentionAction: 'drop',
    })
    await renderApp(table('metrics'))
    await user.click(await screen.findByTestId('policy-run'))
    const dialog = await dialogOf(/^run the policy of public\.metrics now/i)
    const notice = await dialog.findByTestId('run-plan-deletes')
    expect(notice).toHaveTextContent(/the size of the data is not known/i)
    expect(notice).not.toHaveTextContent(/0 B/)
    expect(within(notice).getByTestId('plan-drops').querySelectorAll('li').length).toBeGreaterThan(
      0,
    )
    expect(within(notice).getByTestId('plan-drops')).toHaveTextContent(/metrics_p\d{8}/)
  })

  it('P12W1: a size that the server sent for every dropped partition is shown', async () => {
    const { http, HttpResponse } = await msw()
    sw('analytics=timeseries')
    mockServer.use(
      http.post('*/partition-policy/plan', () =>
        HttpResponse.json({
          asOf: new Date().toISOString(),
          actions: [
            {
              seq: 1,
              kind: 'drop',
              partition: 'a',
              state: 'planned',
              bytesBefore: 3 * 1024 * 1024,
            },
            { seq: 2, kind: 'drop', partition: 'b', state: 'planned', bytesBefore: 1024 * 1024 },
          ],
        }),
      ),
    )
    const user = userEvent.setup()
    await renderApp(table('metrics'))
    await user.click(await screen.findByTestId('policy-run'))
    const dialog = await dialogOf(/^run the policy of public\.metrics now/i)
    expect(await dialog.findByTestId('run-plan-deletes')).toHaveTextContent(
      /the size of the data is 4\.0 MiB/i,
    )
  })

  it('P12W2: a policy that could not be read is never replaced by the defaults', async () => {
    const { http, HttpResponse } = await msw()
    sw('analytics=timeseries')
    mockServer.use(
      http.get('*/partition-policy', () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'x', status: 500, code: 'internal_error' },
          { status: 500 },
        ),
      ),
    )
    const user = userEvent.setup()
    await renderApp(table('metrics'))
    // The panel itself shows the failure; the edit button is not offered over a failed read.
    await waitFor(() =>
      expect(
        within(screen.getByTestId('policy-panel')).getAllByRole('alert').length,
      ).toBeGreaterThan(0),
    )
    expect(screen.queryByTestId('policy-edit')).not.toBeInTheDocument()
    // The list of tables opens the same dialog: its form stays hidden and Save is not there.
    await user.click(screen.getByRole('link', { name: /database shop/i }))
    await user.click(await screen.findByTestId('policy-metrics'))
    const dialog = await dialogOf(/^time partitioning of public\.metrics/i)
    await waitFor(() => expect(dialog.getAllByRole('alert').length).toBeGreaterThan(0))
    expect(dialog.queryByTestId('policy-form')).not.toBeInTheDocument()
    expect(dialog.queryByTestId('policy-save')).not.toBeInTheDocument()
    expect(getState().analytics!.policies['shop/public.metrics']!.req.retention).toBe('P90D')
  })

  it('P12W3: Run now stays off when the plan could not be read, and the panel never sends asOf', async () => {
    const { http, HttpResponse } = await msw()
    sw('analytics=timeseries')
    mockServer.use(
      http.post('*/partition-policy/plan', () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'x', status: 500, code: 'internal_error' },
          { status: 500 },
        ),
      ),
    )
    const user = userEvent.setup()
    await renderApp(table('metrics'))
    await user.click(await screen.findByTestId('policy-run'))
    const dialog = await dialogOf(/^run the policy of public\.metrics now/i)
    await waitFor(() => expect(dialog.getAllByRole('alert').length).toBeGreaterThan(0))
    expect(dialog.getByRole('button', { name: /^run now$/i })).toBeDisabled()
    expect(getState().tasks.some((t) => t.task.kind === 'partition.maintain')).toBe(false)
  })

  it('P12W3 (M8): the request of Run now has an empty body', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    mockConfig.taskDurationMs = 80
    recordBodies('POST', '/partition-policy/runs')
    await renderApp(table('metrics'))
    await user.click(await screen.findByTestId('policy-run'))
    const dialog = await dialogOf(/^run the policy of public\.metrics now/i)
    const start = await dialog.findByRole('button', { name: /^run now$/i })
    await enabled(start)
    await user.click(start)
    await dialog.findByText('Finished.')
    expect(bodies).toEqual([{}])
  })

  it('P12W3: Run now is red when the plan drops, and not when it only creates', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    await renderApp(table('metrics'))
    await user.click(await screen.findByTestId('policy-run'))
    let dialog = await dialogOf(/^run the policy of public\.metrics now/i)
    const start = await dialog.findByRole('button', { name: /^run now$/i })
    await enabled(start)
    expect(start).not.toHaveClass('btn-danger')
    await user.click(dialog.getByRole('button', { name: /^cancel$/i }))
    await signedIn()
    await endpoints.putPartitionPolicy('shop', 'public', 'metrics', {
      partitionEvery: 'P1D',
      retention: 'P5D',
      retentionAction: 'drop',
    })
    await user.click(await screen.findByTestId('policy-run'))
    dialog = await dialogOf(/^run the policy of public\.metrics now/i)
    await dialog.findByTestId('run-plan-deletes')
    await waitFor(() =>
      expect(dialog.getByRole('button', { name: /^run now$/i })).toHaveClass('btn-danger'),
    )
  })

  it('P12W4: turning on a policy that drops needs the preview and the name', async () => {
    sw('analytics=timeseries')
    await signedIn()
    await endpoints.putPartitionPolicy('shop', 'public', 'metrics', {
      partitionEvery: 'P1D',
      retention: 'P5D',
      retentionAction: 'drop',
      enabled: false,
    })
    const user = userEvent.setup()
    await renderApp(table('metrics'))
    await user.click(await screen.findByTestId('policy-edit'))
    const dialog = await dialogOf(/^time partitioning of public\.metrics/i)
    await dialog.findByTestId('policy-form')
    await user.click(dialog.getByTestId('policy-enabled'))
    expect(await dialog.findByTestId('policy-need-preview')).toBeInTheDocument()
    expect(dialog.getByTestId('policy-save')).toBeDisabled()
  })

  it('P12W5: a quarterly policy keeps its interval when only the retention is changed', async () => {
    sw('analytics=timeseries')
    await signedIn()
    await endpoints.putPartitionPolicy('shop', 'public', 'metrics', {
      partitionEvery: 'P3M',
      retention: 'P90D',
      retentionAction: 'drop',
    })
    const user = userEvent.setup()
    recordBodies('PUT', '/partition-policy')
    await renderApp(table('metrics'))
    await user.click(await screen.findByTestId('policy-edit'))
    const dialog = await dialogOf(/^time partitioning of public\.metrics/i)
    expect(await dialog.findByTestId('policy-kept')).toHaveTextContent(
      /P3M.*3 months.*kept as it is/i,
    )
    await clear(user, dialog.getByTestId('policy-retention'), '120')
    const save = dialog.getByTestId('policy-save')
    await enabled(save)
    await user.click(save)
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(bodies[0]).toMatchObject({ partitionEvery: 'P3M', retention: 'P120D' })
  })

  it('P12W5: the person can change a kept value, then the form value is sent', async () => {
    sw('analytics=timeseries')
    await signedIn()
    await endpoints.putPartitionPolicy('shop', 'public', 'metrics', { partitionEvery: 'P6M' })
    const user = userEvent.setup()
    recordBodies('PUT', '/partition-policy')
    await renderApp(table('metrics'))
    await user.click(await screen.findByTestId('policy-edit'))
    const dialog = await dialogOf(/^time partitioning of public\.metrics/i)
    await dialog.findByTestId('policy-kept')
    await user.click(dialog.getByRole('button', { name: /change it/i }))
    await waitFor(() => expect(dialog.queryByTestId('policy-kept')).not.toBeInTheDocument())
    const save = dialog.getByTestId('policy-save')
    await enabled(save)
    await user.click(save)
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(bodies[0]).toMatchObject({ partitionEvery: 'P1D' })
  })

  it('P12W8: an error of the server on /compressionLevel is shown next to its field', async () => {
    const { http, HttpResponse } = await msw()
    sw('analytics=timeseries')
    mockServer.use(
      http.put('*/storage', ({ request }) =>
        new URL(request.url).searchParams.get('dryRun') === 'true'
          ? HttpResponse.json({
              current: 'heap',
              target: 'columnar',
              willConvert: true,
              blockers: [],
              notes: [],
            })
          : HttpResponse.json(
              {
                type: 'about:blank',
                title: 'x',
                status: 400,
                code: 'validation_failed',
                errors: [{ field: '/compressionLevel', message: 'The server wants 1 to 19.' }],
              },
              { status: 400 },
            ),
      ),
    )
    const user = userEvent.setup()
    await renderApp(table('clicks'))
    await user.click(await screen.findByTestId('convert-storage'))
    const dialog = await dialogOf(/^convert storage of table clicks/i)
    const start = dialog.getByRole('button', { name: /^convert$/i })
    await enabled(start)
    await user.click(start)
    expect(
      await dialog.findByText('The server wants 1 to 19.', { selector: '.error' }),
    ).toBeInTheDocument()
  })

  it('P12W10: the unit fields have names of their own', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    await renderApp(table('metrics'))
    await user.click(await screen.findByTestId('policy-edit'))
    const dialog = await dialogOf(/^time partitioning of public\.metrics/i)
    await dialog.findByTestId('policy-form')
    for (const name of [
      'Create ahead unit',
      'Retention unit',
      'Convert-after unit',
      'Run every unit',
    ])
      expect(dialog.getByLabelText(name)).toBeInTheDocument()
    expect(dialog.queryAllByLabelText('Unit')).toHaveLength(0)
  })

  it('P12W10: a 404 that says the table is on another instance is an error, not "no policy"', async () => {
    const { http, HttpResponse } = await msw()
    sw('analytics=timeseries')
    mockServer.use(
      http.get('*/partition-policy', () =>
        HttpResponse.json(
          {
            type: 'about:blank',
            title: 'x',
            status: 404,
            code: 'not_found',
            detail: 'The table is not on the instance of the manager leader (another shard group).',
          },
          { status: 404 },
        ),
      ),
    )
    await renderApp(table('metrics'))
    const panel = within(await screen.findByTestId('policy-panel'))
    await waitFor(() => expect(panel.getAllByRole('alert').length).toBeGreaterThan(0))
    expect(panel.queryByTestId('policy-none')).not.toBeInTheDocument()
    expect(panel.queryByTestId('policy-edit')).not.toBeInTheDocument()
  })

  it('P12W10: a partition whose format is not known is not counted as a row partition', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    await signedIn()
    const list = getState().partitions!['shop/public.metrics']!
    list.find((p) => p.accessMethod === 'heap')!.accessMethod = null
    await renderApp(table('metrics'))
    const rows = within(await screen.findByTestId('partition-list'))
    await user.selectOptions(screen.getByTestId('partition-format'), 'heap')
    expect(rows.getAllByRole('row').slice(1)).toHaveLength(7)
  })
})

describe('the detach path of the preview (P12W9, M17)', () => {
  it('says how many partitions the next run would detach, and does not call it a deletion', async () => {
    sw('analytics=timeseries')
    const user = userEvent.setup()
    await signedIn()
    await endpoints.putPartitionPolicy('shop', 'public', 'metrics', {
      partitionEvery: 'P1D',
      retention: 'P5D',
      retentionAction: 'detach',
    })
    await renderApp(table('metrics'))
    await user.click(await screen.findByTestId('policy-edit'))
    const dialog = await dialogOf(/^time partitioning of public\.metrics/i)
    await dialog.findByTestId('policy-form')
    await user.click(dialog.getByTestId('policy-preview'))
    expect(await dialog.findByTestId('policy-preview-summary')).toHaveTextContent(
      /drop 0 and detach [1-9]\d*/i,
    )
    expect(dialog.queryByTestId('policy-preview-deletes')).not.toBeInTheDocument()
  })
})

describe('a plan that cannot be read again (P12W3, N2)', () => {
  it('holds Run now when a later read of the plan fails, although an older plan is still shown', async () => {
    const { http, HttpResponse } = await import('msw')
    sw('analytics=timeseries')
    const user = userEvent.setup()
    const { queryClient } = await renderApp(table('metrics'))
    await user.click(await screen.findByTestId('policy-run'))
    const dialog = await dialogOf(/^run the policy of public\.metrics now/i)
    const start = await dialog.findByRole('button', { name: /^run now$/i })
    await enabled(start)
    mockServer.use(
      http.post('*/partition-policy/plan', () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'x', status: 500, code: 'internal_error' },
          { status: 500 },
        ),
      ),
    )
    await queryClient.refetchQueries({ queryKey: ['partitionPlan'] })
    await waitFor(() => expect(start).toBeDisabled())
  })
})
