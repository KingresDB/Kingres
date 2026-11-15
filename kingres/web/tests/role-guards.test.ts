// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Contract 0.4.0: reading logs needs kingres_ops. Navigation, the route guard and the mock follow
// `x-required-role` from the generated operation table, never a hand-kept list.
import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/vue'
import { operations } from '@/api/operations.generated'
import { canCall } from '@/api/roles'
import { routes } from '@/router'
import type { ClusterRoleName } from '@/api/types'
import { renderApp } from './helpers'

const ACCOUNTS: [string, ClusterRoleName][] = [
  ['admin', 'kingres_admin'],
  ['ops', 'kingres_ops'],
  ['viewer', 'kingres_readonly'],
  ['metrics', 'kingres_stats'],
]

/** Every child route that names an operation, with the title its navigation entry shows. */
const navRoutes = (routes.find((r) => r.path === '/')?.children ?? [])
  .filter((r) => r.meta?.nav && r.meta.operation)
  .map((r) => ({
    path: r.path,
    title: String(r.meta?.title),
    op: r.meta?.operation as keyof typeof operations,
  }))

describe('the generated table says logs need kingres_ops', () => {
  it('listLogs and listThisServerLogs', () => {
    expect(operations.listLogs.role).toBe('kingres_ops')
    expect(operations.listThisServerLogs.role).toBe('kingres_ops')
  })
})

describe('navigation entries follow x-required-role', () => {
  for (const [user, role] of ACCOUNTS) {
    it(`${user} (${role}) sees exactly the entries it may call`, async () => {
      await renderApp('/dashboard', { user })
      const nav = await screen.findByRole('navigation', { name: 'Main' })
      for (const r of navRoutes) {
        const link = within(nav).queryByRole('link', { name: r.title })
        if (canCall([role], r.op)) expect(link, `${r.title} for ${user}`).toBeInTheDocument()
        else expect(link, `${r.title} for ${user}`).not.toBeInTheDocument()
      }
    })
  }
})

describe('logs pages', () => {
  for (const user of ['viewer', 'metrics']) {
    for (const path of ['/logs', '/this-server/logs']) {
      it(`${user} gets the Forbidden page at ${path} and no Logs entry`, async () => {
        await renderApp(path, { user })
        await screen.findByText('Not allowed')
        const nav = screen.getByRole('navigation', { name: 'Main' })
        expect(within(nav).queryByRole('link', { name: 'Logs' })).not.toBeInTheDocument()
        expect(within(nav).queryByRole('link', { name: 'Local logs' })).not.toBeInTheDocument()
      })
    }
  }
  for (const path of ['/logs', '/this-server/logs']) {
    it(`ops reads the page at ${path}`, async () => {
      const app = await renderApp(path, { user: 'ops' })
      await waitFor(() => expect(app.router.currentRoute.value.name).not.toBe('forbidden'))
      expect(screen.queryByText('Not allowed')).not.toBeInTheDocument()
      const nav = screen.getByRole('navigation', { name: 'Main' })
      expect(
        within(nav).getByRole('link', { name: path === '/logs' ? 'Logs' : 'Local logs' }),
      ).toBeInTheDocument()
    })
  }
})
