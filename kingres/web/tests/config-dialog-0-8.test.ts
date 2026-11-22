// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.8.0, the edit dialog of the Configuration page: the level and its scope (and why a level is off), a value
// control for every type with the value checked as it is typed (units and ranges), the preview of what the change does on
// every server, Apply, the state of every server (applied, pending restart, failed with the text, waiting), Retry,
// Remove this override, the history with Revert and its refusals, and the dialog that restarts what waits.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { mockServer } from '@/mocks/server'
import { getState } from '@/mocks/http'
import {
  bringConfigServerBack,
  failNextApply,
  seedRestartConflict,
  takeConfigServerDown,
} from '@/mocks/config'
import { endpoints } from '@/api/endpoints'
import { renderApp } from './helpers'

type User = ReturnType<typeof userEvent.setup>

async function openEdit(user: User, name: string, path = `/configuration?q=${name}`) {
  const app = await renderApp(path)
  const row = await screen.findByTestId(`param-${name}`)
  await user.click(within(row).getByRole('button', { name: `Edit ${name}` }))
  const dialog = await screen.findByRole('dialog', { name: `Edit ${name}` })
  // The parameter is read again for the dialog.
  await within(dialog).findByRole('group', { name: 'Level' })
  return { app, dialog: within(dialog) }
}

const value = (d: ReturnType<typeof within>) => d.getByLabelText('Value') as HTMLInputElement

async function type(user: User, d: ReturnType<typeof within>, text: string) {
  const input = value(d)
  await user.clear(input)
  if (text) await user.type(input, text)
}

async function ready(d: ReturnType<typeof within>) {
  await waitFor(() =>
    expect(d.getByRole('button', { name: /^(Apply|Revert)$/ })).not.toHaveAttribute(
      'aria-disabled',
    ),
  )
}

describe('opening', () => {
  it('works at once when the parameter is already in the cache (a second visit)', async () => {
    const user = userEvent.setup()
    const app = await renderApp('/configuration?q=work_mem')
    app.queryClient.setQueryData(
      ['config', 'parameter', 'work_mem'],
      await endpoints.getConfigParameter('work_mem'),
    )
    const row = await screen.findByTestId('param-work_mem')
    await user.click(within(row).getByRole('button', { name: 'Edit work_mem' }))
    const d = within(await screen.findByRole('dialog', { name: 'Edit work_mem' }))
    expect(d.getByLabelText('Value')).toHaveValue('8MB')
    expect(d.getByRole('radio', { name: 'Cluster' })).toBeChecked()
  })
})

describe('the level', () => {
  it('offers the four levels, with the scope of the one that is chosen', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'work_mem')
    const levels = d
      .getAllByRole('radio')
      .map((r) => (r as HTMLInputElement).labels?.[0]?.textContent?.trim())
    expect(levels).toEqual(['Cluster', 'Server group', 'Server', 'Database'])
    expect(d.getByRole('radio', { name: 'Cluster' })).toBeChecked()
    expect(d.queryByRole('combobox', { name: 'Server' })).toBeNull()
    await user.click(d.getByRole('radio', { name: 'Server' }))
    const scope = d.getByRole('combobox', { name: 'Server' })
    expect(
      within(scope)
        .getAllByRole('option')
        .map((o) => o.textContent),
    ).toEqual(['10.0.0.11', '10.0.0.12', '10.0.0.13'])
    await user.click(d.getByRole('radio', { name: 'Server group' }))
    expect(
      within(d.getByRole('combobox', { name: 'Server group' }))
        .getAllByRole('option')
        .map((o) => o.textContent),
    ).toEqual(['rack-a', 'rack-b'])
    await user.click(d.getByRole('radio', { name: 'Database' }))
    expect(
      within(d.getByRole('combobox', { name: 'Database' }))
        .getAllByRole('option')
        .map((o) => o.textContent),
    ).toEqual(['shop', 'reports'])
  })

  it('switches off the levels a parameter that must be equal on every server does not have, and says why', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'TimeZone')
    expect(d.getByRole('radio', { name: 'Server group' })).toBeDisabled()
    expect(d.getByRole('radio', { name: 'Server' })).toBeDisabled()
    expect(d.getByRole('radio', { name: 'Cluster' })).toBeEnabled()
    expect(d.getByRole('radio', { name: 'Database' })).toBeEnabled()
    expect(d.getByRole('radio', { name: 'Server' })).toHaveAccessibleDescription(
      'This setting must be equal on every server, so it is set for the cluster.',
    )
  })

  it('has no database level for a setting that PostgreSQL does not allow there', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'shared_buffers')
    const db = d.getByRole('radio', { name: 'Database' })
    expect(db).toBeDisabled()
    expect(db).toHaveAccessibleDescription('PostgreSQL has no database level for this setting.')
  })

  it('shows the value that is set at the chosen level, and keeps the typed value when the level changes', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'work_mem')
    expect(value(d)).toHaveValue('8MB')
    expect(d.getByTestId('current-override')).toHaveTextContent('Set here now: 8MB (revision 6)')
    await user.click(d.getByRole('radio', { name: 'Server' }))
    await user.selectOptions(d.getByRole('combobox', { name: 'Server' }), '10.0.0.11')
    expect(d.queryByTestId('current-override')).toBeNull()
    await user.selectOptions(d.getByRole('combobox', { name: 'Server' }), '10.0.0.13')
    expect(d.getByTestId('current-override')).toHaveTextContent('32MB')
  })
})

describe('the value is checked as it is typed', () => {
  it('says what is wrong in a live region, names the unit and the range, and keeps Apply off', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'work_mem')
    await type(user, d, '63kB')
    const message = d.getByTestId('value-message')
    expect(message).toHaveAttribute('aria-live', 'polite')
    expect(message).toHaveTextContent(/whole number between 64kB and 2147483647kB/)
    expect(value(d)).toHaveAttribute('aria-invalid', 'true')
    expect(value(d)).toHaveAccessibleDescription(/whole number in kB/)
    expect(d.getByRole('button', { name: /^Apply/ })).toHaveAttribute('aria-disabled', 'true')
    await type(user, d, '0x40')
    expect(message).toHaveTextContent(/hexadecimal.*not accepted/i)
    await type(user, d, '16 MB')
    expect(message).toHaveTextContent('')
    expect(value(d)).not.toHaveAttribute('aria-invalid')
  })

  it('has a unit select that rewrites the unit of what was typed', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'work_mem')
    await type(user, d, '16MB')
    const unit = d.getByRole('combobox', { name: 'Unit' })
    expect(unit).toHaveValue('MB')
    await user.selectOptions(unit, 'GB')
    expect(value(d)).toHaveValue('16GB')
    expect(
      within(unit)
        .getAllByRole('option')
        .map((o) => o.textContent),
    ).toEqual(['unit', 'B', 'kB', 'MB', 'GB', 'TB'])
  })

  it('uses a select for a boolean, with on and off', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'jit')
    const select = d.getByLabelText('Value')
    expect(select.tagName).toBe('SELECT')
    expect(
      within(select)
        .getAllByRole('option')
        .map((o) => o.textContent),
    ).toEqual(['on', 'off'])
    expect(select).toHaveValue('on')
    await user.selectOptions(select, 'off')
    expect(await d.findByText(/3 of 3 servers will change/)).toBeInTheDocument()
  })

  it('uses a select for an enum, with the values of the server', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'wal_compression')
    const select = d.getByLabelText('Value')
    expect(
      within(select)
        .getAllByRole('option')
        .map((o) => o.textContent),
    ).toEqual(['pglz', 'lz4', 'zstd', 'on', 'off'])
    expect(d.getByText(/One of: pglz, lz4, zstd, on, off/)).toBeInTheDocument()
  })

  it('checks a real number against its range and says there is no exponent form', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'random_page_cost')
    await type(user, d, '1e3')
    expect(d.getByTestId('value-message')).toHaveTextContent(/exponent/i)
    await type(user, d, '-1')
    expect(d.getByTestId('value-message')).toHaveTextContent(/decimal number at least 0/)
    await type(user, d, '1.1')
    expect(d.getByTestId('value-message')).toHaveTextContent('')
  })

  it('checks a text value for its length and its line breaks', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'log_line_prefix')
    await type(user, d, '"$user", public, extra')
    expect(d.getByTestId('value-message')).toHaveTextContent('')
    expect(d.getByText(/at most 4096 bytes/)).toBeInTheDocument()
  })

  it('checks a time zone against the list of the server', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'TimeZone')
    expect(d.getByLabelText('Time zone')).toHaveValue('Asia/Tehran')
    await user.clear(d.getByLabelText('Time zone'))
    await user.type(d.getByLabelText('Time zone'), 'Mars/Olympus')
    expect(await d.findByText(/does not know this time zone/)).toBeInTheDocument()
    expect(d.getByRole('button', { name: /^Apply/ })).toHaveAttribute('aria-disabled', 'true')
    expect(document.querySelector('#cfg-zones option[value="Asia/Tehran"]')).not.toBeNull()
  })
})

describe('the preview', () => {
  it('says how many servers change and that nothing is restarted, with a line per server', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'work_mem')
    await type(user, d, '16MB')
    expect(await d.findByTestId('preview-sentence')).toHaveTextContent(
      '2 of 3 servers will change. 0 need a restart; nothing is restarted by this change.',
    )
    expect(
      within(d.getByTestId('effect-10.0.0.11')).getByText('Takes effect after a reload'),
    ).toBeInTheDocument()
    expect(within(d.getByTestId('effect-10.0.0.11')).getByText('8MB')).toBeInTheDocument()
    expect(within(d.getByTestId('effect-10.0.0.11')).getByText('16MB')).toBeInTheDocument()
    // The server override beats the cluster value.
    expect(within(d.getByTestId('effect-10.0.0.13')).getByText('No change')).toBeInTheDocument()
    expect(d.queryByTestId('restart-note')).toBeNull()
  })

  it('says which servers need a restart, the order, and the downtime of a primary', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'max_connections')
    await type(user, d, '200')
    expect(await d.findByTestId('preview-sentence')).toHaveTextContent(
      '3 of 3 servers will change. 3 need a restart; nothing is restarted by this change.',
    )
    const note = d.getByTestId('restart-note')
    expect(note).toHaveTextContent('Nothing is restarted by this change')
    expect(note).toHaveTextContent('downtime for its shard group')
    expect(note).toHaveTextContent('Failover is not available yet')
    const steps = within(d.getByTestId('restart-order'))
      .getAllByRole('listitem')
      .map((li) => li.textContent)
    expect(steps.length).toBe(8)
    // A value that goes up: every standby before its primary.
    const roles = steps.map((s) => (s ?? '').includes('(standby)'))
    expect(roles.indexOf(false)).toBeGreaterThan(roles.lastIndexOf(true))
    expect(
      within(d.getByTestId('effect-10.0.0.11')).getByText('Waits for a restart'),
    ).toBeInTheDocument()
  })

  it('says the primaries come first when a value goes down', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'max_connections')
    await type(user, d, '50')
    const steps = within(await d.findByTestId('restart-order'))
      .getAllByRole('listitem')
      .map((li) => li.textContent ?? '')
    expect(steps[0]).toContain('(primary)')
    expect(steps[steps.length - 1]).toContain('(standby)')
  })

  it('shows the refusal of the server in words and keeps Apply off', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'max_wal_senders')
    await type(user, d, '2')
    const problems = await d.findByTestId('preview-problems')
    expect(problems).toHaveTextContent(/max_wal_senders must be at least 5/)
    expect(d.getByRole('button', { name: /^Apply/ })).toHaveAttribute('aria-disabled', 'true')
    expect(d.queryByTestId('preview-sentence')).toBeNull()
  })

  it('shows a server that is down as waiting', async () => {
    takeConfigServerDown(getState(), '10.0.0.12')
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'work_mem')
    await type(user, d, '16MB')
    await d.findByTestId('preview-sentence')
    expect(
      within(d.getByTestId('effect-10.0.0.12')).getByText('Server is down: will wait'),
    ).toBeInTheDocument()
  })

  it('says that no server would change when the value is the one that runs', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'work_mem')
    expect(await d.findByText('No server would change.')).toBeInTheDocument()
    expect(d.getByRole('button', { name: /^Apply/ })).toHaveAttribute('aria-disabled', 'true')
  })

  it('keeps one preview of the last thing typed, not of every key', async () => {
    let calls = 0
    mockServer.use(
      http.post('*/api/v1/config/changes/preview', async ({ request }) => {
        calls += 1
        const body = (await request.json()) as { changes: { value: string }[] }
        return HttpResponse.json({
          valid: true,
          problems: [],
          effects: [
            {
              serverId: '10.0.0.11',
              name: 'work_mem',
              before: '8MB',
              after: body.changes[0]!.value,
              takesEffect: 'reload',
            },
          ],
          restartServerIds: [],
          restartOrder: [],
          restartOrderConflict: null,
        })
      }),
    )
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'work_mem')
    await type(user, d, '16MB')
    await d.findByTestId('preview-sentence')
    expect(calls).toBeLessThanOrEqual(2)
    expect(within(d.getByTestId('effect-10.0.0.11')).getByText('16MB')).toBeInTheDocument()
  })
})

describe('applying', () => {
  it('follows the task and shows the state of every server', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'work_mem')
    await type(user, d, '16MB')
    await ready(d)
    await user.click(d.getByRole('button', { name: /^Apply/ }))
    expect(await d.findByRole('region', { name: /task \d+ progress/i })).toBeInTheDocument()
    for (const id of ['10.0.0.11', '10.0.0.12', '10.0.0.13'])
      await waitFor(() =>
        expect(
          within(d.getByTestId(`server-state-${id}`)).getByText('Applied'),
        ).toBeInTheDocument(),
      )
    expect(await d.findByText('Applied on every server.')).toBeInTheDocument()
    expect(getState().config.revision).toBe(7)
    await user.click(d.getByRole('button', { name: /^(Close|Hide \(keeps running\))$/ }))
    // The page behind it shows the new value.
    await waitFor(() => expect(screen.getAllByText('16MB').length).toBeGreaterThanOrEqual(2))
  })

  it('shows a server that waits for a restart as pending, with what it waits for', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'max_connections')
    await type(user, d, '200')
    await ready(d)
    await user.click(d.getByRole('button', { name: /^Apply/ }))
    for (const id of ['10.0.0.11', '10.0.0.12', '10.0.0.13']) {
      const r = await d.findByTestId(`server-state-${id}`)
      await waitFor(() => expect(within(r).getByText('Pending restart')).toBeInTheDocument())
      expect(r).toHaveTextContent('Waits for a restart: max_connections')
    }
    expect(
      await d.findByText(/Some servers wait for a restart; nothing was restarted/),
    ).toBeInTheDocument()
    expect(getState().config.servers['10.0.0.11']!.run.max_connections!.value).toBe('100')
  })

  it('shows a server that failed with the text, and Retry sends the same change again', async () => {
    failNextApply(getState(), '10.0.0.12', 'invalid value for parameter "work_mem": "16MB"')
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'work_mem')
    await type(user, d, '16MB')
    await ready(d)
    await user.click(d.getByRole('button', { name: /^Apply/ }))
    const failed = await d.findByTestId('server-state-10.0.0.12')
    await waitFor(() => expect(within(failed).getByText('Failed')).toBeInTheDocument())
    expect(failed).toHaveTextContent('invalid value for parameter "work_mem": "16MB"')
    expect(within(d.getByTestId('server-state-10.0.0.11')).getByText('Applied')).toBeInTheDocument()
    const alerts = await d.findAllByRole('alert')
    expect(alerts.some((a) => /failed on 10\.0\.0\.12/.test(a.textContent ?? ''))).toBe(true)
    // Only the server that failed has Retry.
    expect(d.getAllByRole('button', { name: /^Retry/ })).toHaveLength(1)
    await user.click(d.getByRole('button', { name: 'Retry on 10.0.0.12' }))
    await waitFor(() =>
      expect(
        within(d.getByTestId('server-state-10.0.0.12')).getByText('Applied'),
      ).toBeInTheDocument(),
    )
    expect(d.queryByRole('button', { name: /^Retry/ })).toBeNull()
    // The retry is the same change, not a second one.
    expect(getState().config.revision).toBe(7)
  })

  it('shows a server that waits, lets the task be cancelled, and goes on when the server is back', async () => {
    takeConfigServerDown(getState(), '10.0.0.13')
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'work_mem')
    await type(user, d, '16MB')
    await ready(d)
    await user.click(d.getByRole('button', { name: /^Apply/ }))
    const r = await d.findByTestId('server-state-10.0.0.13')
    await waitFor(() => expect(within(r).getByText('Waiting for the server')).toBeInTheDocument())
    expect(r).toHaveTextContent('This server cannot be reached; it gets the change when it returns')
    expect(await d.findByRole('button', { name: 'Cancel the task' })).toBeInTheDocument()
    bringConfigServerBack(getState(), '10.0.0.13')
    await waitFor(() =>
      expect(
        within(d.getByTestId('server-state-10.0.0.13')).getByText('Applied'),
      ).toBeInTheDocument(),
    )
  })

  it('offers Retry for a server that waits after the task was cancelled', async () => {
    takeConfigServerDown(getState(), '10.0.0.13')
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'work_mem')
    await type(user, d, '16MB')
    await ready(d)
    await user.click(d.getByRole('button', { name: /^Apply/ }))
    await user.click(await d.findByRole('button', { name: 'Cancel the task' }))
    await waitFor(() =>
      expect(d.getByRole('button', { name: 'Retry on 10.0.0.13' })).toBeInTheDocument(),
    )
  })

  it('can be closed while the task runs, and says that it keeps running', async () => {
    takeConfigServerDown(getState(), '10.0.0.13')
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'work_mem')
    await type(user, d, '16MB')
    await ready(d)
    await user.click(d.getByRole('button', { name: /^Apply/ }))
    await user.click(await d.findByRole('button', { name: 'Hide (keeps running)' }))
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(getState().tasks.find((t) => t.task.kind === 'config.apply')!.task.state).toBe('running')
  })

  it('shows the refusal of the server when the start is refused', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'work_mem')
    await type(user, d, '16MB')
    await ready(d)
    mockServer.use(
      http.post('*/api/v1/config/changes', () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'x', status: 409, code: 'operation_in_progress' },
          { status: 409 },
        ),
      ),
    )
    await user.click(d.getByRole('button', { name: /^Apply/ }))
    expect((await d.findByRole('alert')).getAttribute('data-code')).toBe('operation_in_progress')
    expect(d.getByRole('button', { name: /^Apply/ })).toBeInTheDocument()
  })

  it('sends the revision it saw when it opened, and a refusal as superseded says to look again (P13R-11)', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'work_mem')
    await type(user, d, '16MB')
    await ready(d)
    const seen = getState().config.revision
    const bodies: { expectedRevision?: number }[] = []
    mockServer.use(
      http.post('*/api/v1/config/changes', async ({ request }) => {
        bodies.push((await request.json()) as { expectedRevision?: number })
        return HttpResponse.json(
          { type: 'about:blank', title: 'x', status: 409, code: 'config_change_superseded' },
          { status: 409 },
        )
      }),
    )
    await user.click(d.getByRole('button', { name: /^Apply/ }))
    expect((await d.findByRole('alert')).getAttribute('data-code')).toBe('config_change_superseded')
    expect(bodies).toHaveLength(1)
    expect(bodies[0]!.expectedRevision).toBe(seen)
  })
})

describe('removing an override', () => {
  it('previews the fall back to the lower level and removes the override', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'work_mem')
    await user.click(d.getByRole('radio', { name: 'Server' }))
    await user.selectOptions(d.getByRole('combobox', { name: 'Server' }), '10.0.0.13')
    await user.click(d.getByRole('button', { name: 'Remove this override' }))
    expect(d.queryByLabelText('Value')).toBeNull()
    expect(d.getByRole('button', { name: 'Keep the override' })).toBeInTheDocument()
    await d.findByTestId('preview-sentence')
    const r = within(d.getByTestId('effect-10.0.0.13'))
    expect(r.getByText('32MB')).toBeInTheDocument()
    expect(r.getByText('8MB')).toBeInTheDocument()
    await ready(d)
    await user.click(d.getByRole('button', { name: /^Apply/ }))
    await waitFor(() =>
      expect(
        within(d.getByTestId('server-state-10.0.0.13')).getByText('Applied'),
      ).toBeInTheDocument(),
    )
    expect(
      getState().config.overrides.some((o) => o.level === 'server' && o.name === 'work_mem'),
    ).toBe(false)
  })

  it('says there is nothing to remove where nothing is set', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'work_mem')
    await user.click(d.getByRole('radio', { name: 'Server' }))
    await user.selectOptions(d.getByRole('combobox', { name: 'Server' }), '10.0.0.11')
    expect(d.queryByRole('button', { name: 'Remove this override' })).toBeNull()
  })
})

describe('the history and revert', () => {
  it('lists the changes, newest first, and offers Revert for the latest of a level and scope only', async () => {
    await renderApp('/configuration?tab=changes')
    const first = await screen.findByTestId('change-6')
    expect(within(first).getByText('16MB')).toBeInTheDocument()
    expect(within(first).getByText('8MB')).toBeInTheDocument()
    expect(within(first).getByRole('button', { name: 'Revert change 6' })).toBeInTheDocument()
    const old = screen.getByTestId('change-2')
    expect(within(old).queryByRole('button', { name: /revert/i })).toBeNull()
    expect(within(old).getByText('Superseded: a later change exists.')).toHaveAttribute(
      'title',
      expect.stringMatching(/revert the later one first/i),
    )
    const ids = screen.getAllByTestId(/^change-/).map((e) => e.getAttribute('data-testid'))
    expect(ids).toEqual(['change-6', 'change-5', 'change-4', 'change-3', 'change-2', 'change-1'])
    expect(screen.getByTestId('change-4')).toHaveTextContent(/Server\s*10\.0\.0\.13/)
  })

  it('filters by parameter name', async () => {
    const user = userEvent.setup()
    await renderApp('/configuration?tab=changes')
    await screen.findByTestId('change-6')
    await user.type(
      screen.getByRole('searchbox', { name: /parameter name/i }),
      'effective_io_concurrency',
    )
    await waitFor(() => expect(screen.getAllByTestId(/^change-/)).toHaveLength(1))
    expect(screen.getByTestId('change-3')).toBeInTheDocument()
  })

  it('opens the same dialog, filled with the earlier value, previews it and applies it as a new change', async () => {
    const user = userEvent.setup()
    await renderApp('/configuration?tab=changes')
    await user.click(await screen.findByRole('button', { name: 'Revert change 6' }))
    const dialog = await screen.findByRole('dialog', { name: 'Revert change 6' })
    const d = within(dialog)
    expect(await d.findByTestId('revert-summary')).toHaveTextContent(
      /back to 16MB \(it is 8MB now\)/,
    )
    expect(await d.findByTestId('preview-sentence')).toHaveTextContent(
      '2 of 3 servers will change.',
    )
    expect(d.queryByLabelText('Value')).toBeNull()
    await ready(d)
    await user.click(d.getByRole('button', { name: /^Revert$|^Revert Apply/ }))
    await waitFor(() =>
      expect(
        within(d.getByTestId('server-state-10.0.0.11')).getByText('Applied'),
      ).toBeInTheDocument(),
    )
    await user.click(d.getByRole('button', { name: /^(Close|Hide \(keeps running\))$/ }))
    const newest = await screen.findByTestId('change-7')
    expect(within(newest).getByText('reverts change 6')).toBeInTheDocument()
    expect(
      within(screen.getByTestId('change-6')).queryByRole('button', { name: /revert/i }),
    ).toBeNull()
    expect(within(newest).getByRole('button', { name: 'Revert change 7' })).toBeInTheDocument()
  })

  it('puts back "no override" when there was none', async () => {
    const user = userEvent.setup()
    await renderApp('/configuration?tab=changes&name=effective_io_concurrency')
    await user.click(await screen.findByRole('button', { name: 'Revert change 3' }))
    const d = within(await screen.findByRole('dialog', { name: 'Revert change 3' }))
    expect(await d.findByTestId('revert-summary')).toHaveTextContent(/back to no override/)
    await d.findByTestId('preview-sentence')
    await ready(d)
    await user.click(d.getByRole('button', { name: /^Revert$|^Revert Apply/ }))
    await waitFor(() =>
      expect(
        within(d.getByTestId('server-state-10.0.0.11')).getByText('Applied'),
      ).toBeInTheDocument(),
    )
    expect(getState().config.overrides.some((o) => o.name === 'effective_io_concurrency')).toBe(
      false,
    )
  })

  it('explains a change that was superseded in the meantime', async () => {
    const user = userEvent.setup()
    await renderApp('/configuration?tab=changes')
    await user.click(await screen.findByRole('button', { name: 'Revert change 6' }))
    const d = within(await screen.findByRole('dialog', { name: 'Revert change 6' }))
    await d.findByTestId('preview-sentence')
    await ready(d)
    mockServer.use(
      http.post('*/api/v1/config/changes/:id/revert', () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'x', status: 409, code: 'config_change_superseded' },
          { status: 409 },
        ),
      ),
    )
    await user.click(d.getByRole('button', { name: /^Revert$|^Revert Apply/ }))
    const notice = await d.findByRole('alert')
    expect(notice).toHaveTextContent('A later change exists')
    expect(notice).toHaveTextContent(/revert the later change first/i)
  })

  it('shows Revert disabled with the reason to a read-only account', async () => {
    await renderApp('/configuration?tab=changes', { user: 'viewer' })
    const button = await screen.findByRole('button', { name: 'Revert change 6' })
    expect(button).toHaveAttribute('aria-disabled', 'true')
    expect(button).toHaveAccessibleDescription(/requires the kingres_admin cluster role/i)
  })

  it('says when there is no change yet, and pages the history', async () => {
    getState().config.changes = []
    await renderApp('/configuration?tab=changes')
    expect(await screen.findByText('No changes yet')).toBeInTheDocument()
  })
})

describe('restarting what waits', () => {
  async function open(user: User, role = 'admin') {
    await renderApp('/configuration', { user: role })
    await user.click(await screen.findByRole('button', { name: /restart pending instances/i }))
    return within(await screen.findByRole('dialog', { name: 'Restart pending instances' }))
  }

  it('says that restarting a primary is downtime and that failover is not available, and waits for the acknowledgement', async () => {
    const user = userEvent.setup()
    const d = await open(user)
    const note = d.getByTestId('downtime-note')
    expect(note).toHaveTextContent('Restarting a primary is downtime for its shard group')
    expect(note).toHaveTextContent('Failover is not available yet, so nothing takes over')
    expect(await d.findByTestId('waiting-servers')).toHaveTextContent(
      '10.0.0.12: waits for shared_buffers',
    )
    const box = d.getByRole('checkbox', { name: /I understand/ })
    expect(box).not.toBeChecked()
    const go = d.getByRole('button', { name: /^Restart now/ })
    expect(go).toHaveAttribute('aria-disabled', 'true')
    await user.click(go)
    expect(getState().tasks.some((t) => t.task.kind === 'config.restart')).toBe(false)
    await user.click(box)
    expect(go).not.toHaveAttribute('aria-disabled')
  })

  it('restarts, shows the steps in the order of the server and then says it is done', async () => {
    const user = userEvent.setup()
    const d = await open(user)
    await d.findByTestId('waiting-servers')
    await user.click(d.getByRole('checkbox', { name: /I understand/ }))
    await user.click(d.getByRole('button', { name: /^Restart now/ }))
    expect(await d.findByRole('region', { name: /task \d+ progress/i })).toBeInTheDocument()
    const steps = await d.findAllByText(
      /Restart instance \d+ \((primary|standby)\) on server 10\.0\.0\.12/,
    )
    expect(steps.length).toBeGreaterThan(0)
    expect(await d.findByText(/Every instance was restarted/)).toBeInTheDocument()
    expect(getState().config.servers['10.0.0.12']!.pending).toEqual({})
  })

  it('has nothing to restart when nothing waits', async () => {
    getState().config.servers['10.0.0.12']!.pending = {}
    const user = userEvent.setup()
    await renderApp('/configuration')
    const button = await screen.findByRole('button', { name: /restart pending instances/i })
    await waitFor(() => expect(button).toHaveAttribute('aria-disabled', 'true'))
    expect(button).toHaveAccessibleDescription('No instance waits for a restart.')
    await user.click(button)
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('shows the refusal for a mixed direction with the settings named', async () => {
    seedRestartConflict(getState())
    const user = userEvent.setup()
    const d = await open(user)
    await d.findByTestId('waiting-servers')
    await user.click(d.getByRole('checkbox', { name: /I understand/ }))
    await user.click(d.getByRole('button', { name: /^Restart now/ }))
    const notice = await d.findByRole('alert')
    expect(notice).toHaveAttribute('data-code', 'config_restart_order_conflict')
    expect(notice).toHaveTextContent('No safe restart order')
    expect(notice).toHaveTextContent(/max_connections.*max_wal_senders/)
  })

  it('is for administrators', async () => {
    const user = userEvent.setup()
    await renderApp('/configuration', { user: 'ops' })
    const button = await screen.findByRole('button', { name: /restart pending instances/i })
    expect(button).toHaveAttribute('aria-disabled', 'true')
    await user.click(button)
    expect(screen.queryByRole('dialog')).toBeNull()
  })
})

describe('the warnings of the preview (contract 0.17.0)', () => {
  it('shows an unusual memory value as a warning and leaves Apply on', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'work_mem')
    await type(user, d, '16MB')
    await d.findByTestId('preview-sentence')
    expect(d.queryByTestId('preview-warnings')).toBeNull()
    await type(user, d, '1GB')
    const box = await d.findByTestId('preview-warnings')
    expect(box).toHaveTextContent('more than ten times its default')
    await ready(d)
  })

  it('warns that reserved connections leave no room, and still lets the change be applied', async () => {
    const user = userEvent.setup()
    const { dialog: d } = await openEdit(user, 'superuser_reserved_connections')
    await type(user, d, '100')
    const box = await d.findByTestId('preview-warnings')
    expect(box).toHaveTextContent('max_connections (100) would be below')
    await ready(d)
  })
})
