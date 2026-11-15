// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import userEvent from '@testing-library/user-event'
import { canCall, denialReason, highestRole, requiredRoleFor, roleSatisfies } from '@/api/roles'
import { operations } from '@/api/operations.generated'
import { renderApp, settled } from './helpers'

describe('role hierarchy', () => {
  it('follows admin > ops > readonly > stats', () => {
    expect(roleSatisfies(['kingres_admin'], 'kingres_ops')).toBe(true)
    expect(roleSatisfies(['kingres_ops'], 'kingres_admin')).toBe(false)
    expect(roleSatisfies(['kingres_readonly'], 'kingres_stats')).toBe(true)
    expect(roleSatisfies(['kingres_stats'], 'kingres_readonly')).toBe(false)
    expect(roleSatisfies([], 'kingres_stats')).toBe(false)
    expect(roleSatisfies([], 'none')).toBe(true)
    expect(highestRole(['kingres_stats', 'kingres_ops'])).toBe('kingres_ops')
  })

  it('takes the required role of every operation from the contract', () => {
    expect(requiredRoleFor('failoverServer')).toBe('kingres_ops')
    expect(requiredRoleFor('createDatabase')).toBe('kingres_admin')
    expect(requiredRoleFor('queryMetrics')).toBe('kingres_stats')
    expect(canCall(['kingres_ops'], 'createDatabase')).toBe(false)
    expect(canCall(['kingres_readonly'], 'listServers')).toBe(true)
    expect(denialReason('createDatabase')).toContain('kingres_admin')
    expect(Object.keys(operations).length).toBeGreaterThanOrEqual(82)
  })
})

describe('role-based UI', () => {
  it('hides navigation items the role cannot use (read-only user)', async () => {
    await renderApp('/dashboard', { user: 'viewer' })
    const nav = await screen.findByRole('navigation', { name: 'Main' })
    expect(within(nav).getByRole('link', { name: 'Servers' })).toBeInTheDocument()
    expect(within(nav).getByRole('link', { name: 'Databases' })).toBeInTheDocument()
    // Security needs kingres_admin (listRoles), so it must not be offered.
    expect(within(nav).queryByRole('link', { name: 'Security' })).not.toBeInTheDocument()
    expect(within(nav).getByRole('heading', { name: 'Cluster' })).toBeInTheDocument()
    expect(within(nav).getByRole('heading', { name: 'This server' })).toBeInTheDocument()
  })

  it('shows everything to an administrator', async () => {
    await renderApp('/dashboard', { user: 'admin' })
    const nav = await screen.findByRole('navigation', { name: 'Main' })
    expect(within(nav).getByRole('link', { name: 'Security' })).toBeInTheDocument()
    expect(within(nav).getByRole('link', { name: 'Ledger' })).toBeInTheDocument()
  })

  it('disables an action with an explanation instead of hiding it', async () => {
    const user = userEvent.setup()
    await renderApp('/servers', { user: 'viewer' })
    await settled()
    const add = await screen.findByRole('button', { name: /add server/i })
    expect(add).toHaveAttribute('aria-disabled', 'true')
    expect(add).toHaveAccessibleDescription(/requires the kingres_admin cluster role/i)
    await user.click(add)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('enables the action for an administrator only (0.6.0: addServer needs kingres_admin)', async () => {
    const user = userEvent.setup()
    await renderApp('/servers', { user: 'admin' })
    const add = await screen.findByRole('button', { name: /add server/i })
    expect(add).not.toHaveAttribute('aria-disabled')
    await user.click(add)
    expect(await screen.findByRole('dialog', { name: /add server/i })).toBeInTheDocument()
  })

  it('keeps an operator out of admin-only pages', async () => {
    const { router } = await renderApp('/security', { user: 'ops' })
    await waitFor(() => expect(router.currentRoute.value.name).toBe('forbidden'))
    expect(await screen.findByText(/your account cannot open this page/i)).toBeInTheDocument()
  })

  it('redirects a signed-out visitor to the sign-in page', async () => {
    const { router } = await renderApp('/servers', { user: null })
    await waitFor(() => expect(router.currentRoute.value.name).toBe('login'))
    expect(router.currentRoute.value.query.redirect).toBe('/servers')
    expect(
      await screen.findByRole('heading', { name: /sign in to kingresdb/i }),
    ).toBeInTheDocument()
  })
})
