// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Routes are lazy-loaded so that each page is its own chunk. Each route names the contract
// operation it is built on (`meta.operation`); the navigation and the route guard use the
// operation's `x-required-role` (generated) to decide whether the user may see the page.
import type { Pinia } from 'pinia'
import { createRouter, createWebHistory, type RouteRecordRaw, type Router } from 'vue-router'
import type { QueryClient } from '@tanstack/vue-query'
import type { OperationId } from '@/api/roles'
import { endpoints } from '@/api/endpoints'
import { keys } from '@/api/queries'
import { useSessionStore } from '@/stores/session'
import { isAddressableSegment } from '@/lib/path-segment'

declare module 'vue-router' {
  interface RouteMeta {
    title?: string
    /** The operation the page is built on; its required role gates the page. */
    operation?: OperationId
    /** Navigation group; routes without it are not listed in the side navigation. */
    nav?: 'cluster' | 'server'
    /** Reachable without a session. */
    public?: boolean
  }
}

export const routes: RouteRecordRaw[] = [
  {
    path: '/login',
    name: 'login',
    component: () => import('@/pages/LoginPage.vue'),
    meta: { title: 'Sign in', public: true },
  },
  {
    path: '/setup',
    name: 'setup',
    component: () => import('@/pages/SetupPage.vue'),
    meta: { title: 'Set up this server', public: true },
  },
  {
    path: '/',
    component: () => import('@/layouts/ShellLayout.vue'),
    children: [
      { path: '', redirect: { name: 'dashboard' } },
      {
        path: 'dashboard',
        name: 'dashboard',
        component: () => import('@/pages/DashboardPage.vue'),
        // Needs only the metrics role, so a kingres_stats account has a real landing page; the
        // cards on the page that need more are hidden for lower roles.
        meta: { title: 'Dashboard', operation: 'queryMetrics', nav: 'cluster' },
      },
      {
        path: 'servers',
        name: 'servers',
        component: () => import('@/pages/ServersPage.vue'),
        meta: { title: 'Servers', operation: 'listServers', nav: 'cluster' },
      },
      {
        path: 'servers/:id',
        name: 'server',
        component: () => import('@/pages/ServerDetailPage.vue'),
        props: (route) => ({ id: String(route.params.id) }),
        meta: { title: 'Server', operation: 'getServer' },
      },
      {
        path: 'databases',
        name: 'databases',
        component: () => import('@/pages/DatabasesPage.vue'),
        meta: { title: 'Databases', operation: 'listDatabases', nav: 'cluster' },
      },
      {
        path: 'databases/:name',
        name: 'database',
        component: () => import('@/pages/DatabaseDetailPage.vue'),
        props: true,
        meta: { title: 'Database', operation: 'getDatabase' },
      },
      {
        path: 'shards',
        name: 'shards',
        component: () => import('@/pages/ShardsPage.vue'),
        meta: { title: 'Shards', operation: 'getBucketDistribution', nav: 'cluster' },
      },
      {
        path: 'replication',
        name: 'replication',
        component: () => import('@/pages/ReplicationPage.vue'),
        meta: { title: 'Replication', operation: 'getReplicationTopology', nav: 'cluster' },
      },
      {
        path: 'rebalance',
        name: 'rebalance',
        component: () => import('@/pages/RebalancePage.vue'),
        meta: { title: 'Rebalance', operation: 'getRebalanceStatus', nav: 'cluster' },
      },
      {
        path: 'security',
        name: 'security',
        component: () => import('@/pages/SecurityPage.vue'),
        meta: { title: 'Security', operation: 'listRoles', nav: 'cluster' },
      },
      {
        path: 'ledger',
        name: 'ledger',
        component: () => import('@/pages/LedgerPage.vue'),
        meta: { title: 'Ledger', operation: 'listLedgerStatus', nav: 'cluster' },
      },
      {
        path: 'workbench',
        name: 'workbench',
        component: () => import('@/pages/WorkbenchPage.vue'),
        meta: { title: 'Query workbench', operation: 'executeQuery', nav: 'cluster' },
      },
      {
        path: 'tasks',
        name: 'tasks',
        component: () => import('@/pages/TasksPage.vue'),
        meta: { title: 'Tasks', operation: 'listTasks', nav: 'cluster' },
      },
      {
        path: 'alerts',
        name: 'alerts',
        component: () => import('@/pages/AlertsPage.vue'),
        meta: { title: 'Alerts', operation: 'listAlerts', nav: 'cluster' },
      },
      {
        path: 'logs',
        name: 'logs',
        component: () => import('@/pages/LogsPage.vue'),
        meta: { title: 'Logs', operation: 'listLogs', nav: 'cluster' },
      },
      {
        path: 'settings',
        name: 'settings',
        component: () => import('@/pages/SettingsPage.vue'),
        meta: { title: 'Settings', operation: 'getClusterSettings', nav: 'cluster' },
      },
      {
        path: 'this-server',
        name: 'this-server',
        component: () => import('@/pages/ThisServerPage.vue'),
        meta: { title: 'This server', operation: 'getThisServer', nav: 'server' },
      },
      {
        path: 'this-server/logs',
        name: 'this-server-logs',
        component: () => import('@/pages/LogsPage.vue'),
        props: { local: true },
        meta: { title: 'Local logs', operation: 'listThisServerLogs', nav: 'server' },
      },
      {
        path: 'forbidden',
        name: 'forbidden',
        component: () => import('@/pages/ForbiddenPage.vue'),
        meta: { title: 'Not allowed' },
      },
      {
        path: ':rest(.*)*',
        name: 'not-found',
        component: () => import('@/pages/NotFoundPage.vue'),
        meta: { title: 'Page not found' },
      },
    ],
  },
]

/**
 * Creates the router. The guard runs, in order: setup state (wizard while `uninitialized`),
 * session restore, sign-in requirement, and the role needed by the page.
 */
export function createAppRouter(
  queryClient: QueryClient,
  pinia: Pinia,
  history = createWebHistory(),
): Router {
  const router = createRouter({ history, routes })

  router.beforeEach(async (to) => {
    // A name that no URL can carry (`.`, `..`, empty) cannot be opened: the browser would resolve the
    // address to another page. Whatever made the link, it lands on the list, which says so.
    if (to.name === 'database' && !isAddressableSegment(String(to.params.name ?? '')))
      return { name: 'databases' }
    let uninitialized = false
    try {
      const node = await queryClient.fetchQuery({
        queryKey: keys.nodeState,
        queryFn: endpoints.getNodeState,
        staleTime: 30_000,
        // Navigation must not hang while the client is paused (system database stopped).
        networkMode: 'always',
      })
      uninitialized = node.state === 'uninitialized'
    } catch {
      // Cannot read the setup state: carry on; the pages show their own errors.
    }
    if (uninitialized) return to.name === 'setup' ? true : { name: 'setup' }
    if (to.name === 'setup') return { name: 'login' }

    // The store is passed explicitly: the guard must not depend on the global active pinia.
    const session = useSessionStore(pinia)
    if (!session.restored) await session.restore()

    if (to.meta.public) {
      return to.name === 'login' && session.isSignedIn ? { name: 'dashboard' } : true
    }
    if (!session.isSignedIn) {
      return { name: 'login', query: to.fullPath === '/' ? {} : { redirect: to.fullPath } }
    }
    if (to.meta.operation && !session.can(to.meta.operation)) return { name: 'forbidden' }
    return true
  })

  router.afterEach((to) => {
    document.title = to.meta.title ? `${to.meta.title} - KingresDB` : 'KingresDB'
  })

  return router
}
