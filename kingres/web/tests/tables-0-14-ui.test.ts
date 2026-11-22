// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The table page and the dialogs of the table lifecycle (contract 0.14.0, increment 9.7): distribute with its mode, bucket count and views,
// undistribute, alter the distribution, the partitions (create, attach, detach, drop), the preview with its blockers and the 422
// `table_not_eligible`, the progress, the failure and the cancel of the four kinds of task, and what each role sees.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { axe } from 'vitest-axe'
import { mockServer } from '@/mocks/server'
import { endpoints } from '@/api/endpoints'
import { getState } from '@/mocks/http'
import { mockConfig } from '@/mocks/state'
import { applySwitches } from '@/mocks/switches'
import { renderApp } from './helpers'

const sw = (q: string) => applySwitches(new URLSearchParams(q), getState())
type User = ReturnType<typeof userEvent.setup>
const page = (db: string, table: string) => `/databases/${db}/tables/public/${table}`
const shopTable = (name: string) =>
  getState()
    .databases.find((d) => d.info.name === 'shop')!
    .tables.find((t) => t.name === name)!
const dialogOf = async (name: RegExp) => within(await screen.findByRole('dialog', { name }))
const enabled = async (button: HTMLElement) => waitFor(() => expect(button).toBeEnabled())

describe('the table page', () => {
  it('shows a distributed table: its class, distribution, columns, and the changes it can take', async () => {
    await renderApp(page('shop', 'orders'))
    expect(
      await screen.findByRole('heading', { level: 1, name: /table public\.orders/i }),
    ).toBeInTheDocument()
    const distribution = await screen.findByTestId('table-distribution')
    expect(distribution).toHaveTextContent(
      /hash of customer_id, in 128 buckets, colocation group 1/i,
    )
    expect(
      within(screen.getByRole('table', { name: /columns of the table/i })).getAllByRole('row'),
    ).toHaveLength(5)
    expect(screen.getByRole('button', { name: /^undistribute table/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^change the distribution/i })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^distribute table/i })).not.toBeInTheDocument()
    expect(screen.getByTestId('not-partitioned')).toBeInTheDocument()
  })

  it('shows a plain table with its home group and the button to distribute it', async () => {
    await renderApp(page('shop', 'notes'))
    expect(await screen.findByTestId('table-home')).toHaveTextContent('Group 0')
    expect(screen.getByRole('button', { name: /^distribute table/i })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^undistribute/i })).not.toBeInTheDocument()
    expect(screen.queryByTestId('table-partitions')).not.toBeInTheDocument()
  })

  it('a table that does not exist says so, with a way to try again', async () => {
    await renderApp(page('shop', 'nothing'))
    expect((await screen.findAllByText(/does not exist|not found/i)).length).toBeGreaterThan(0)
  })

  it('is reached from the list of tables of the database', async () => {
    const user = userEvent.setup()
    await renderApp('/databases/shop')
    await user.click(await screen.findByRole('link', { name: 'public.customers' }))
    expect(
      await screen.findByRole('heading', { level: 1, name: /table public\.customers/i }),
    ).toBeInTheDocument()
  })

  it.each([
    ['viewer', /kingres_admin/],
    ['ops', /kingres_admin/],
  ])('%s sees the change buttons disabled with the reason', async (user, reason) => {
    await renderApp(page('shop', 'orders'), { user })
    const button = await screen.findByRole('button', { name: /^undistribute table/i })
    await waitFor(() => expect(button).toHaveAttribute('aria-disabled', 'true'))
    expect(button).toHaveTextContent(reason)
    expect(screen.getByRole('button', { name: /^change the distribution/i })).toHaveAttribute(
      'aria-disabled',
      'true',
    )
  })

  it('has no accessibility violations', async () => {
    const { container } = await renderApp(page('shop', 'orders'))
    await screen.findByTestId('table-distribution')
    expect(await axe(container)).toHaveNoViolations()
  })
})

describe('distribute', () => {
  async function open(user: User, db = 'shop', table = 'notes') {
    await renderApp(page(db, table))
    await user.click(await screen.findByRole('button', { name: /^distribute table/i }))
    return dialogOf(/^distribute public\./i)
  }

  it('says how many rows are copied, offers the mode, the buckets and the views, and sends only what was chosen', async () => {
    const user = userEvent.setup()
    mockConfig.taskDurationMs = 80
    const bodies: unknown[] = []
    mockServer.events.on('request:start', async ({ request }) => {
      if (request.method === 'PUT' && request.url.endsWith('/distribution'))
        bodies.push(await request.clone().json())
    })
    const dialog = await open(user)
    expect(await dialog.findByTestId('distribute-rows')).toHaveTextContent(/about 1,203 rows/i)
    await dialog.findByTestId('table-change-eligible')
    await user.click(dialog.getByRole('radio', { name: /^blocking/i }))
    await user.type(dialog.getByTestId('distribute-buckets'), '64')
    const start = dialog.getByRole('button', { name: /^distribute table$/i })
    await enabled(start)
    await user.click(start)
    expect(await dialog.findByText('Finished.')).toBeInTheDocument()
    expect(bodies).toEqual([
      {
        method: 'hash',
        distributionColumn: 'note_id',
        colocateWith: null,
        bucketCount: 64,
        mode: 'blocking',
      },
    ])
    expect(shopTable('notes').distribution).toMatchObject({ bucketCount: 64 })
  })

  it('a bucket count outside 1 to 4096 is refused before anything is sent', async () => {
    const user = userEvent.setup()
    const dialog = await open(user)
    await user.type(dialog.getByTestId('distribute-buckets'), '5000')
    expect(await dialog.findByText(/from 1 to 4096/i, { selector: '.error' })).toBeInTheDocument()
    await user.clear(dialog.getByTestId('distribute-buckets'))
    await user.type(dialog.getByTestId('distribute-buckets'), 'abc')
    expect(await dialog.findByText(/whole number/i)).toBeInTheDocument()
  })

  it('a view that uses the table stops the start until the views are to be made again', async () => {
    const user = userEvent.setup()
    getState().databases.find((d) => d.info.name === 'reports')!.info.sharding.enabled = true
    const dialog = await open(user, 'reports', 'monthly_totals')
    const blockers = await dialog.findByTestId('table-blockers')
    expect(blockers).toHaveTextContent(/a view or another object depends on the table/i)
    expect(blockers).toHaveTextContent(/monthly_totals_by_region/)
    expect(dialog.getByRole('button', { name: /^distribute table$/i })).toBeDisabled()
    await user.click(dialog.getByRole('radio', { name: /^make the views again/i }))
    expect(await dialog.findByTestId('table-warnings')).toHaveTextContent(
      /made again on the new table/i,
    )
    await waitFor(() => expect(dialog.queryByTestId('table-blockers')).not.toBeInTheDocument())
    await enabled(dialog.getByRole('button', { name: /^distribute table$/i }))
  })

  it('when the server cannot preview, the change can still be started and a refusal lists its blockers', async () => {
    const user = userEvent.setup()
    getState().databases.find((d) => d.info.name === 'reports')!.info.sharding.enabled = true
    mockServer.use(
      http.post('*/distribution/preview', () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'x', status: 422, code: 'unsupported_operation' },
          { status: 422 },
        ),
      ),
    )
    const dialog = await open(user, 'reports', 'exports')
    expect(await dialog.findByTestId('table-change-unavailable')).toBeInTheDocument()
    const start = dialog.getByRole('button', { name: /^distribute table$/i })
    await enabled(start)
    await user.click(start)
    const refusal = await dialog.findByTestId('table-change-refusal')
    expect(refusal).toHaveTextContent(/this table cannot take the change/i)
    expect(within(refusal).getByTestId('table-blockers')).toHaveTextContent(/exports_audit/)
    expect(within(refusal).getByTestId('table-blockers')).toHaveTextContent(
      /a trigger on the table/i,
    )
    expect(getState().tasks.some((t) => t.task.kind === 'table.distribute')).toBe(false)
  })

  it('a failed task says why, at which step, and the dialog closes with the table as it was', async () => {
    const user = userEvent.setup()
    sw('tables=fail:space')
    mockConfig.taskDurationMs = 80
    const dialog = await open(user)
    const start = dialog.getByRole('button', { name: /^distribute table$/i })
    await enabled(start)
    await user.click(start)
    expect(
      await dialog.findByText(
        /not enough free space on server 10\.0\.0\.13/i,
        {},
        { timeout: 30_000 },
      ),
    ).toBeInTheDocument()
    expect(dialog.getAllByText('Check free space').length).toBeGreaterThan(0)
    expect(dialog.queryByTestId('cancel-task')).not.toBeInTheDocument()
    expect(shopTable('notes').distribution).toBeNull()
    expect(await axe(screen.getByRole('dialog'))).toHaveNoViolations()
  })

  it('Cancel the task stops a copy and says the table is as it was', async () => {
    const user = userEvent.setup()
    sw('tables=slow')
    const dialog = await open(user)
    const start = dialog.getByRole('button', { name: /^distribute table$/i })
    await enabled(start)
    await user.click(start)
    await user.click(await dialog.findByTestId('cancel-task'))
    expect(await dialog.findByTestId('task-cancelled')).toHaveTextContent(
      /table is as it was before/i,
    )
    expect(dialog.queryByTestId('cancel-task')).not.toBeInTheDocument()
    expect(shopTable('notes').distribution).toBeNull()
  })

  it('has no accessibility violations with the preview showing', async () => {
    const user = userEvent.setup()
    const dialog = await open(user)
    await dialog.findByTestId('table-change-eligible')
    expect(await axe(screen.getByRole('dialog'))).toHaveNoViolations()
  })
})

describe('undistribute', () => {
  async function open(user: User, table = 'customers') {
    await renderApp(page('shop', table))
    await user.click(await screen.findByRole('button', { name: /^undistribute table/i }))
    return dialogOf(/^undistribute public\./i)
  }

  it('names what stops the table, and the cascade and the views lift it', async () => {
    const user = userEvent.setup()
    mockConfig.taskDurationMs = 80
    const urls: string[] = []
    mockServer.events.on('request:start', ({ request }) => {
      if (request.method === 'DELETE') urls.push(request.url)
    })
    const dialog = await open(user)
    const blockers = await dialog.findByTestId('table-blockers')
    expect(blockers).toHaveTextContent(/a foreign key links the table to another/i)
    expect(blockers).toHaveTextContent(/public\.orders/)
    expect(dialog.getByRole('button', { name: /^undistribute table$/i })).toBeDisabled()
    await user.click(dialog.getByTestId('undistribute-cascade'))
    await user.click(dialog.getByRole('radio', { name: /^make the views again/i }))
    const affected = await dialog.findByTestId('table-change-affected')
    await waitFor(() => expect(affected).toHaveTextContent(/public\.orders/))
    expect(affected).toHaveTextContent(/linked by a foreign key/i)
    await waitFor(() => expect(dialog.queryByTestId('table-blockers')).not.toBeInTheDocument())
    await user.selectOptions(dialog.getByTestId('undistribute-group'), '2')
    const start = dialog.getByRole('button', { name: /^undistribute table$/i })
    await enabled(start)
    await user.click(start)
    expect(await dialog.findByText('Finished.')).toBeInTheDocument()
    const query = new URL(urls[0] as string).searchParams
    expect(Object.fromEntries(query)).toEqual({
      cascadeViaForeignKeys: 'true',
      targetGroupId: '2',
      dependentViews: 'recreate',
    })
    expect(shopTable('orders').distribution).toBeNull()
    expect(shopTable('customers').homeShardGroupId).toBe(2)
  })

  it('an online copy that the table cannot have is shown with its reason and cannot be chosen', async () => {
    const user = userEvent.setup()
    mockServer.use(
      http.post('*/distribution/preview', () =>
        HttpResponse.json({
          blockers: [],
          warnings: [],
          notes: [],
          affectedTables: [],
          estimate: {
            rows: 10,
            bytes: 100,
            modeRecommended: 'blocking',
            onlineAvailable: false,
            onlineUnavailableReason: 'No key on NOT NULL columns.',
            writersBlocked: 'wholeCopy',
          },
        }),
      ),
    )
    const dialog = await open(user, 'events')
    const reason = await dialog.findByTestId('table-change-online-reason')
    expect(reason).toHaveTextContent(/no key on not null columns/i)
    expect(dialog.getByRole('radio', { name: /^online/i })).toBeDisabled()
    expect(dialog.getByTestId('table-change-writers')).toHaveTextContent(/whole copy/i)
  })
})

describe('alter the distribution', () => {
  async function open(user: User, table = 'orders') {
    await renderApp(page('shop', table))
    await user.click(await screen.findByRole('button', { name: /^change the distribution/i }))
    return dialogOf(/^change the distribution of public\./i)
  }

  it('asks for something to change, and sends only what changed', async () => {
    const user = userEvent.setup()
    mockConfig.taskDurationMs = 80
    const bodies: unknown[] = []
    mockServer.events.on('request:start', async ({ request }) => {
      if (request.method === 'PATCH') bodies.push(await request.clone().json())
    })
    const dialog = await open(user, 'events')
    expect(await dialog.findByTestId('alter-current')).toHaveTextContent(/session_id.*128 buckets/i)
    await user.click(dialog.getByRole('button', { name: /^change the distribution$/i }))
    expect(await dialog.findByTestId('alter-nothing')).toHaveTextContent(/choose what to change/i)
    expect(bodies).toEqual([])
    await user.type(dialog.getByTestId('alter-buckets'), '256')
    expect(await dialog.findByTestId('alter-multiple')).toHaveTextContent(/no row moves/i)
    const start = dialog.getByRole('button', { name: /^change the distribution$/i })
    await enabled(start)
    await user.click(start)
    expect(await dialog.findByText('Finished.')).toBeInTheDocument()
    expect(bodies).toEqual([{ bucketCount: 256 }])
  })

  it('a change of the colocated tables together is blocking only', async () => {
    const user = userEvent.setup()
    const dialog = await open(user)
    await user.type(await dialog.findByTestId('alter-buckets'), '100')
    await user.click(dialog.getByTestId('alter-cascade'))
    expect(dialog.getByRole('radio', { name: /^blocking/i })).toBeChecked()
    expect(dialog.getByRole('radio', { name: /^online/i })).toBeDisabled()
    expect(dialog.getByRole('radio', { name: /^automatic/i })).toBeDisabled()
    expect(dialog.getByTestId('table-change-blocking-only')).toBeInTheDocument()
    await waitFor(() =>
      expect(dialog.getByTestId('table-change-affected')).toHaveTextContent(/public\.customers/),
    )
  })

  it('a user-partitioned table cannot be altered, and the page says why', async () => {
    sw('tables=partitions')
    await renderApp(page('shop', 'measurements'))
    const button = await screen.findByRole('button', { name: /^change the distribution/i })
    await waitFor(() => expect(button).toHaveAttribute('aria-disabled', 'true'))
    expect(button).toHaveTextContent(/cannot be altered in this version/i)
    expect(screen.getByRole('button', { name: /^undistribute table/i })).toHaveTextContent(
      /cannot be undistributed in this version/i,
    )
  })

  it('has no accessibility violations', async () => {
    const user = userEvent.setup()
    const dialog = await open(user)
    await user.type(await dialog.findByTestId('alter-buckets'), '100')
    await dialog.findByTestId('table-change-estimate')
    expect(await axe(screen.getByRole('dialog'))).toHaveNoViolations()
  })
})

describe('partitions', () => {
  it('lists them with the bound, the leaves and the size, and marks the default one', async () => {
    sw('tables=partitions')
    await renderApp(page('shop', 'measurements'))
    expect(await screen.findByTestId('table-partitioning')).toHaveTextContent(
      /RANGE on taken_at, 3 partitions/,
    )
    const list = await screen.findByTestId('partition-list')
    expect(within(list).getAllByRole('row')).toHaveLength(4)
    const first = within(list).getByTestId('partition-measurements_2026_01')
    expect(first).toHaveTextContent("FROM ('2026-01-01') TO ('2026-02-01')")
    expect(first).toHaveTextContent('32')
    expect(within(list).getByTestId('partition-measurements_default')).toHaveTextContent('Default')
    expect(screen.getByTestId('partitioned-badge')).toBeInTheDocument()
  })

  it('creates a partition: the name and the bound are checked, then the list grows', async () => {
    const user = userEvent.setup()
    sw('tables=partitions')
    mockConfig.taskDurationMs = 80
    await renderApp(page('shop', 'measurements'))
    await user.click(await screen.findByTestId('create-partition'))
    const dialog = await dialogOf(/^create a partition of/i)
    await user.click(dialog.getByRole('button', { name: /^create partition$/i }))
    expect(await dialog.findByText(/give the partition a name/i)).toBeInTheDocument()
    expect(dialog.getByText(/give the bound/i)).toBeInTheDocument()
    await user.type(dialog.getByTestId('partition-name'), 'x'.repeat(58))
    expect(
      await dialog.findByText(/at most 57 bytes: the name/i, { selector: '.error' }),
    ).toBeInTheDocument()
    await user.clear(dialog.getByTestId('partition-name'))
    await user.type(dialog.getByTestId('partition-name'), 'measurements_2026_03')
    await user.type(dialog.getByTestId('partition-bound'), "FROM ('2026-03-01') TO ('2026-04-01')")
    await dialog.findByTestId('table-change-eligible')
    const start = dialog.getByRole('button', { name: /^create partition$/i })
    await enabled(start)
    await user.click(start)
    expect(await dialog.findByText('Finished.')).toBeInTheDocument()
    await user.click(dialog.getByRole('button', { name: /^close$/i }))
    expect(await screen.findByTestId('partition-measurements_2026_03')).toBeInTheDocument()
  })

  it('a partition that exists is refused with 409 in words', async () => {
    const user = userEvent.setup()
    sw('tables=partitions')
    await renderApp(page('shop', 'measurements'))
    await user.click(await screen.findByTestId('create-partition'))
    const dialog = await dialogOf(/^create a partition of/i)
    await user.type(dialog.getByTestId('partition-name'), 'measurements_2026_01')
    await user.type(dialog.getByTestId('partition-bound'), 'DEFAULT')
    const start = dialog.getByRole('button', { name: /^create partition$/i })
    await enabled(start)
    await user.click(start)
    expect(await dialog.findByRole('alert')).toHaveTextContent(/already exists|exists already/i)
  })

  it('attaches only a table of the same colocation group and column', async () => {
    const user = userEvent.setup()
    sw('tables=partitions')
    mockConfig.taskDurationMs = 80
    await renderApp(page('shop', 'measurements'))
    await user.click(await screen.findByTestId('attach-partition'))
    const dialog = await dialogOf(/^attach a table to/i)
    const options = within(dialog.getByTestId('attach-table')).getAllByRole('option')
    expect(options.map((o) => o.textContent?.trim())).toEqual([
      'Choose a table',
      'public.measurements_old',
    ])
    await user.click(dialog.getByRole('button', { name: /^attach table$/i }))
    expect(await dialog.findByText(/choose the table to attach/i)).toBeInTheDocument()
    await user.selectOptions(dialog.getByTestId('attach-table'), options[1] as HTMLElement)
    await user.type(dialog.getByTestId('partition-bound'), "FROM ('2025-01-01') TO ('2026-01-01')")
    await dialog.findByTestId('table-change-eligible')
    const start = dialog.getByRole('button', { name: /^attach table$/i })
    await enabled(start)
    await user.click(start)
    expect(await dialog.findByText('Finished.')).toBeInTheDocument()
    expect(getState().partitions?.['shop/public.measurements']?.map((p) => p.name)).toContain(
      'measurements_old',
    )
  })

  it('detaches a partition without deleting rows, and the preview can stop it', async () => {
    const user = userEvent.setup()
    sw('tables=partitions,fk:measurements')
    await renderApp(page('shop', 'measurements'))
    await user.click(await screen.findByTestId('detach-measurements_2026_01'))
    const dialog = await dialogOf(/^detach partition measurements_2026_01/i)
    expect(dialog.getByTestId('detach-partition-text')).toHaveTextContent(
      /no row moves and none is deleted/i,
    )
    expect(await dialog.findByTestId('table-blockers')).toHaveTextContent(
      /foreign keys cannot detach a partition/i,
    )
    expect(dialog.getByRole('button', { name: /^detach partition$/i })).toBeDisabled()
  })

  it('dropping needs the name of the partition typed and says its rows are deleted', async () => {
    const user = userEvent.setup()
    sw('tables=partitions')
    mockConfig.taskDurationMs = 80
    await renderApp(page('shop', 'measurements'))
    await user.click(await screen.findByTestId('drop-measurements_default'))
    const dialog = await dialogOf(/^drop partition measurements_default/i)
    expect(dialog.getByTestId('drop-partition-warning')).toHaveTextContent(/its rows are deleted/i)
    const confirm = dialog.getByRole('button', { name: /^drop partition$/i })
    expect(confirm).toBeDisabled()
    await user.type(dialog.getByLabelText(/to confirm/i), 'measurements_default')
    await enabled(confirm)
    await user.click(confirm)
    expect(await dialog.findByText('Finished.')).toBeInTheDocument()
    await user.click(dialog.getByRole('button', { name: /^close$/i }))
    await waitFor(() =>
      expect(screen.queryByTestId('partition-measurements_default')).not.toBeInTheDocument(),
    )
  })

  it('a role that may not change partitions sees the buttons disabled with the reason', async () => {
    sw('tables=partitions')
    await renderApp(page('shop', 'measurements'), { user: 'viewer' })
    const create = await screen.findByTestId('create-partition')
    await waitFor(() => expect(create).toHaveAttribute('aria-disabled', 'true'))
    expect(create).toHaveTextContent(/kingres_admin/)
    expect(await screen.findByTestId('drop-measurements_default')).toHaveAttribute(
      'aria-disabled',
      'true',
    )
  })

  it('a hostile partition name is shown with its hidden characters visible', async () => {
    sw('tables=partitions')
    const list = getState().partitions!['shop/public.measurements']!
    list[0] = { ...list[0]!, name: 'p‮evil', bound: "IN ('‮')" }
    await renderApp(page('shop', 'measurements'))
    const rows = await screen.findByTestId('partition-list')
    await waitFor(() => expect(rows.textContent).toMatch(/U\+202E/))
    expect(rows.textContent).not.toMatch(/‮/)
  })

  it('has no accessibility violations', async () => {
    sw('tables=partitions')
    const { container } = await renderApp(page('shop', 'measurements'))
    await screen.findByTestId('partition-list')
    expect(await axe(container)).toHaveNoViolations()
  })
})

describe('the changes of a table that run, and have run', () => {
  it('shows the running task with its steps and Cancel, and holds the other changes', async () => {
    const user = userEvent.setup()
    sw('tables=slow')
    await renderApp(page('shop', 'events'))
    await screen.findByTestId('table-distribution')
    const task = await endpoints.undistributeTable('shop', 'public', 'events')
    const running = await screen.findByTestId('table-task-running', {}, { timeout: 30_000 })
    expect(running).toHaveTextContent(/undistribute a table/i)
    expect(within(running).getByText('Check free space')).toBeInTheDocument()
    const alter = screen.getByRole('button', { name: /^change the distribution/i })
    await waitFor(() => expect(alter).toHaveAttribute('aria-disabled', 'true'))
    expect(alter).toHaveTextContent(new RegExp(`running \\(task ${task.id}\\)`))
    await user.click(within(running).getByTestId('table-task-cancel'))
    const failed = await screen.findByTestId('table-task-failed')
    expect(failed).toHaveTextContent(/cancelled/i)
    expect(failed).toHaveTextContent(/table is as it was before/i)
    expect(failed).toHaveAttribute('role', 'status')
  })

  it('a task past its switch cannot be cancelled, and the button says so', async () => {
    sw('tables=slow')
    await renderApp(page('shop', 'events'))
    await screen.findByTestId('table-distribution')
    const task = await endpoints.undistributeTable('shop', 'public', 'events')
    const rec = getState().tasks.find((t) => t.task.id === task.id)!
    rec.startedAt = Date.now() - 0.8 * rec.durationMs
    const reason = await screen.findByTestId('table-task-cancel-reason', {}, { timeout: 30_000 })
    expect(reason).toHaveTextContent(/goes on to its end/i)
    expect(screen.getByTestId('table-task-cancel')).toHaveAttribute('aria-disabled', 'true')
  })

  it('a failed task is told in words, with its step and the message of the server', async () => {
    sw('tables=fail:copy')
    mockConfig.taskDurationMs = 80
    await renderApp(page('shop', 'events'))
    await screen.findByTestId('table-distribution')
    await endpoints.undistributeTable('shop', 'public', 'events')
    const failed = await screen.findByTestId('table-task-failed', {}, { timeout: 30_000 })
    expect(failed).toHaveAttribute('role', 'alert')
    expect(failed).toHaveTextContent(/undistribute a table: failed/i)
    expect(failed).toHaveTextContent(/bucket 41 failed/i)
    expect(failed).toHaveTextContent(/at the step copy rows/i)
  })

  it('the last change that finished is named; an old failure is not shown after a later success', async () => {
    mockConfig.taskDurationMs = 80
    sw('tables=fail:copy')
    await renderApp(page('shop', 'events'))
    await screen.findByTestId('table-distribution')
    await endpoints.undistributeTable('shop', 'public', 'events')
    await screen.findByTestId('table-task-failed', {}, { timeout: 30_000 })
    mockConfig.tableFail = null
    await new Promise((r) => setTimeout(r, 1100))
    const second = await endpoints.undistributeTable('shop', 'public', 'events')
    await waitFor(async () => expect((await endpoints.getTask(second.id)).state).toBe('done'), {
      timeout: 30_000,
    })
    expect(await screen.findByTestId('table-task-done', {}, { timeout: 30_000 })).toHaveTextContent(
      /undistribute a table, finished/i,
    )
    await waitFor(() => expect(screen.queryByTestId('table-task-failed')).not.toBeInTheDocument(), {
      timeout: 30_000,
    })
  })
})
