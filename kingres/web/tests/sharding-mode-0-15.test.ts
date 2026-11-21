// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.15.0, the sharding mode of a database (design 14.1): the create dialog with Rows, Schemas and Both, the conditional
// bucket count and the default for new schemas; the settings dialog that changes the mode (and shows the refusal
// `sharding_mode_in_use`) and turns sharding on with a mode; the roles that cannot update see text.
import { afterEach, describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import EditDatabaseDialog from '@/components/EditDatabaseDialog.vue'
import { getState } from '@/mocks/http'
import { mockServer } from '@/mocks/server'
import type { Database } from '@/api/types'
import { renderApp, renderComponent, settled } from './helpers'

afterEach(() => mockServer.events.removeAllListeners())

/** The JSON bodies of the requests that match, in order. */
function recordBodies(method: string, path: string): Record<string, unknown>[] {
  const bodies: Record<string, unknown>[] = []
  mockServer.events.on('request:start', ({ request }) => {
    if (request.method !== method || new URL(request.url).pathname !== `/api/v1${path}`) return
    void request
      .clone()
      .json()
      .then((b: Record<string, unknown>) => bodies.push(b))
  })
  return bodies
}

async function openCreate() {
  const user = userEvent.setup()
  await renderApp('/databases')
  await settled()
  await user.click(await screen.findByRole('button', { name: /create database/i }))
  const dialog = await screen.findByRole('dialog', { name: /create database/i })
  // The choices come from the server; wait for them.
  await within(dialog).findByLabelText(/encoding/i)
  return { user, dialog }
}

const shardingBox = (dialog: HTMLElement) =>
  within(dialog).getByRole('checkbox', { name: /spread this database/i })
const mode = (dialog: HTMLElement, name: RegExp) => within(dialog).getByRole('radio', { name })

describe('Create database: Shard by', () => {
  it('offers no mode while sharding is off, then Rows (the default), Schemas and Both with their sentences', async () => {
    const { user, dialog } = await openCreate()
    expect(within(dialog).queryByRole('radio')).not.toBeInTheDocument()
    await user.click(shardingBox(dialog))
    const group = within(dialog).getByRole('group', { name: 'Shard by' })
    expect(within(group).getAllByRole('radio')).toHaveLength(3)
    expect(mode(dialog, /^rows:/i)).toBeChecked()
    expect(mode(dialog, /^schemas:/i)).not.toBeChecked()
    expect(mode(dialog, /^both:/i)).not.toBeChecked()
    expect(group).toHaveTextContent('a table is split into buckets by a column')
    expect(group).toHaveTextContent('each schema (a tenant or a service) lives on one server group')
    expect(group).toHaveTextContent('choose per table or per schema')
  })

  it('Rows: shows the bucket count and no default for new schemas', async () => {
    const { user, dialog } = await openCreate()
    await user.click(shardingBox(dialog))
    expect(within(dialog).getByLabelText(/bucket count/i)).toBeEnabled()
    expect(
      within(dialog).queryByRole('checkbox', { name: /distribute new schemas automatically/i }),
    ).not.toBeInTheDocument()
  })

  it('Schemas: hides the bucket count and offers the default for new schemas, checked, with its sentence', async () => {
    const { user, dialog } = await openCreate()
    await user.click(shardingBox(dialog))
    await user.click(mode(dialog, /^schemas:/i))
    expect(within(dialog).queryByLabelText(/bucket count/i)).not.toBeInTheDocument()
    const box = within(dialog).getByRole('checkbox', {
      name: /distribute new schemas automatically/i,
    })
    expect(box).toBeChecked()
    expect(
      within(dialog).getByText(
        /every schema created in this database is placed on a server group/i,
      ),
    ).toHaveTextContent(
      /schemas created by an extension, public and system schemas are not distributed/i,
    )
  })

  it('Both: shows the bucket count and the default for new schemas, unchecked', async () => {
    const { user, dialog } = await openCreate()
    await user.click(shardingBox(dialog))
    await user.click(mode(dialog, /^both:/i))
    expect(within(dialog).getByLabelText(/bucket count/i)).toBeEnabled()
    expect(
      within(dialog).getByRole('checkbox', { name: /distribute new schemas automatically/i }),
    ).not.toBeChecked()
  })

  it('goes back to the default of a mode when the mode changes', async () => {
    const { user, dialog } = await openCreate()
    await user.click(shardingBox(dialog))
    await user.click(mode(dialog, /^schemas:/i))
    await user.click(mode(dialog, /^both:/i))
    expect(
      within(dialog).getByRole('checkbox', { name: /distribute new schemas automatically/i }),
    ).not.toBeChecked()
    await user.click(mode(dialog, /^schemas:/i))
    expect(
      within(dialog).getByRole('checkbox', { name: /distribute new schemas automatically/i }),
    ).toBeChecked()
  })

  it('Schemas: sends the mode and the default and no bucket count, and the database is created in that mode', async () => {
    const bodies = recordBodies('POST', '/databases')
    const { user, dialog } = await openCreate()
    await user.type(within(dialog).getByLabelText(/^name/i), 'tenant_db')
    await user.click(shardingBox(dialog))
    await user.click(mode(dialog, /^schemas:/i))
    await user.click(within(dialog).getByRole('button', { name: /^create database$/i }))
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()
    await waitFor(() => expect(bodies).toHaveLength(1))
    expect(bodies[0]?.sharding).toEqual({
      enabled: true,
      mode: 'schema',
      distributeNewSchemas: true,
    })
    const created = getState().databases.find((d) => d.info.name === 'tenant_db')
    expect(created?.info.sharding).toMatchObject({
      enabled: true,
      mode: 'schema',
      distributeNewSchemas: true,
      bucketCount: null,
    })
  })

  it('Both: sends the bucket count, the mode and the default that was chosen', async () => {
    const bodies = recordBodies('POST', '/databases')
    const { user, dialog } = await openCreate()
    await user.type(within(dialog).getByLabelText(/^name/i), 'mixed_db')
    await user.click(shardingBox(dialog))
    await user.click(mode(dialog, /^both:/i))
    await user.click(
      within(dialog).getByRole('checkbox', { name: /distribute new schemas automatically/i }),
    )
    await user.click(within(dialog).getByRole('button', { name: /^create database$/i }))
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()
    await waitFor(() => expect(bodies).toHaveLength(1))
    expect(bodies[0]?.sharding).toEqual({
      enabled: true,
      bucketCount: 128,
      mode: 'both',
      distributeNewSchemas: true,
    })
  })

  it('Rows: sends neither a mode nor a default (what a server before 0.15.0 also understands)', async () => {
    const bodies = recordBodies('POST', '/databases')
    const { user, dialog } = await openCreate()
    await user.type(within(dialog).getByLabelText(/^name/i), 'rows_db')
    await user.click(shardingBox(dialog))
    await user.click(within(dialog).getByRole('button', { name: /^create database$/i }))
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()
    await waitFor(() => expect(bodies).toHaveLength(1))
    expect(bodies[0]?.sharding).toEqual({ enabled: true, bucketCount: 128 })
  })

  it('does not check the bucket count in the mode Schemas', async () => {
    const { user, dialog } = await openCreate()
    await user.type(within(dialog).getByLabelText(/^name/i), 'tenant_db')
    await user.click(shardingBox(dialog))
    const buckets = within(dialog).getByLabelText(/bucket count/i)
    await user.clear(buckets)
    await user.type(buckets, '5000')
    await user.click(mode(dialog, /^schemas:/i))
    await user.click(within(dialog).getByRole('button', { name: /^create database$/i }))
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()
  })

  it('shows no choice when the server offers no modes (a server before 0.15.0)', async () => {
    mockServer.use(
      http.get('*/api/v1/database-create-options', () =>
        HttpResponse.json({
          encodings: [{ name: 'UTF8', isDefault: true }],
          collations: [{ name: 'C', encodings: null, isDefault: true }],
          bucketCount: { default: 128, minimum: 1, maximum: 4096 },
          replicaCount: 1,
          defaultDurability: 'sync',
          ledgerDigestIntervalSeconds: { default: 3600, minimum: 60, maximum: 86400 },
        }),
      ),
    )
    const { user, dialog } = await openCreate()
    await user.click(shardingBox(dialog))
    expect(within(dialog).queryByRole('radio')).not.toBeInTheDocument()
    expect(within(dialog).getByLabelText(/bucket count/i)).toBeEnabled()
  })
})

async function openSettings(name: string, user = 'admin') {
  const u = userEvent.setup()
  await renderApp(`/databases/${name}`, { user })
  await settled()
  await u.click(await screen.findByRole('button', { name: /edit settings/i }))
  const dialog = await screen.findByRole('dialog', { name: new RegExp(`edit ${name}`, 'i') })
  await within(dialog).findByRole('group', { name: 'Shard by' })
  return { user: u, dialog }
}

describe('Database settings: the mode', () => {
  it('shows the mode and the default of a database that distributes schemas, and the bucket count as not used', async () => {
    const { dialog } = await openSettings('tenants')
    expect(mode(dialog, /^schemas:/i)).toBeChecked()
    expect(
      within(dialog).getByRole('checkbox', { name: /distribute new schemas automatically/i }),
    ).toBeChecked()
    expect(within(dialog).getByText(/not used in the mode schemas/i)).toBeInTheDocument()
  })

  it('refuses Rows while distributed schemas exist, and says why in the problem notice', async () => {
    const { user, dialog } = await openSettings('tenants')
    await user.click(mode(dialog, /^rows:/i))
    await user.click(within(dialog).getByRole('button', { name: /^save changes$/i }))
    const notice = await within(dialog).findByRole('alert')
    expect(notice).toHaveAttribute('data-code', 'sharding_mode_in_use')
    expect(notice).toHaveTextContent(
      /this database has distributed schemas, so it cannot be set to rows only/i,
    )
    expect(getState().databases.find((d) => d.info.name === 'tenants')?.info.sharding.mode).toBe(
      'schema',
    )
  })

  it('changes Schemas to Both, keeps the default for new schemas and sends only the mode', async () => {
    const bodies = recordBodies('PATCH', '/databases/tenants')
    const { user, dialog } = await openSettings('tenants')
    await user.click(mode(dialog, /^both:/i))
    expect(
      within(dialog).getByRole('checkbox', { name: /distribute new schemas automatically/i }),
    ).toBeChecked()
    await user.click(within(dialog).getByRole('button', { name: /^save changes$/i }))
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()
    await waitFor(() => expect(bodies).toHaveLength(1))
    expect(bodies[0]).toEqual({ sharding: { mode: 'both' } })
    const info = getState().databases.find((d) => d.info.name === 'tenants')?.info
    expect(info?.sharding).toMatchObject({ mode: 'both', distributeNewSchemas: true })
  })

  it('turns the default for new schemas off', async () => {
    const bodies = recordBodies('PATCH', '/databases/tenants')
    const { user, dialog } = await openSettings('tenants')
    await user.click(
      within(dialog).getByRole('checkbox', { name: /distribute new schemas automatically/i }),
    )
    await user.click(within(dialog).getByRole('button', { name: /^save changes$/i }))
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()
    await waitFor(() => expect(bodies).toHaveLength(1))
    expect(bodies[0]).toEqual({ sharding: { distributeNewSchemas: false } })
  })

  it('turns sharding on with a mode', async () => {
    const bodies = recordBodies('PATCH', '/databases/reports')
    const { user, dialog } = await (async () => {
      const u = userEvent.setup()
      await renderApp('/databases/reports')
      await settled()
      await u.click(await screen.findByRole('button', { name: /edit settings/i }))
      const d = await screen.findByRole('dialog', { name: /edit reports/i })
      return { user: u, dialog: d }
    })()
    expect(within(dialog).queryByRole('radio')).not.toBeInTheDocument()
    await user.click(within(dialog).getByRole('checkbox', { name: /turn on sharding/i }))
    await user.click(await within(dialog).findByRole('radio', { name: /^schemas:/i }))
    expect(
      within(dialog).getByRole('checkbox', { name: /distribute new schemas automatically/i }),
    ).toBeChecked()
    await user.click(within(dialog).getByRole('button', { name: /^save changes$/i }))
    expect(await within(dialog).findByText('Finished.')).toBeInTheDocument()
    await waitFor(() => expect(bodies).toHaveLength(1))
    expect(bodies[0]).toEqual({
      sharding: { enabled: true, mode: 'schema', distributeNewSchemas: true },
    })
    const info = getState().databases.find((d) => d.info.name === 'reports')?.info
    expect(info?.sharding).toMatchObject({ enabled: true, mode: 'schema' })
  })

  it('shows the mode and the default as text for a role that cannot update the database', async () => {
    const info = structuredClone(
      getState().databases.find((d) => d.info.name === 'tenants')?.info,
    ) as Database
    await renderComponent(EditDatabaseDialog, { database: info }, { user: 'viewer' })
    const dialog = await screen.findByRole('dialog', { name: /edit tenants/i })
    await waitFor(() =>
      expect(within(dialog).getByTestId('mode-text')).toHaveTextContent('Schemas'),
    )
    expect(within(dialog).getByTestId('distribute-new-text')).toHaveTextContent('Yes')
    expect(within(dialog).queryByRole('radio')).not.toBeInTheDocument()
    expect(
      within(dialog).queryByRole('checkbox', { name: /distribute new schemas automatically/i }),
    ).not.toBeInTheDocument()
  })
})

describe('The list of databases tells the mode', () => {
  it('lists the mode in the sharding column', async () => {
    await renderApp('/databases')
    await settled()
    const row = (await screen.findByRole('link', { name: 'tenants' })).closest('tr') as HTMLElement
    expect(within(row).getByText(/on, schemas/i)).toBeInTheDocument()
    const shop = screen.getByRole('link', { name: 'shop' }).closest('tr') as HTMLElement
    expect(within(shop).getByText(/on, 128 buckets/i)).toBeInTheDocument()
  })
})
