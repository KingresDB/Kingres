// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// Server state: TanStack Query hooks over the endpoint functions. Query keys are defined once in
// `keys` so that mutations can invalidate them precisely. Polling intervals are chosen per
// resource: fast-changing state is refreshed while the tab is visible, the rest on demand.
import { computed, toValue, type MaybeRefOrGetter } from 'vue'
import {
  MutationCache,
  QueryClient,
  QueryCache,
  useInfiniteQuery,
  useQuery,
  type QueryKey,
} from '@tanstack/vue-query'
import { endpoints } from './endpoints'
import { ApiError, isSessionLost, isUnsupported, shouldRetry } from './problem'
import type { MetricName } from './types'

/**
 * How many pages a "load everything" query follows. A server that keeps answering with a cursor
 * must not keep the browser busy for ever; what is left over is reported (`moreNotShown`).
 */
export const paging = { maxPages: 20 }

interface CursorPage<T> {
  items: T[]
  nextCursor?: string | null
}

export interface AllPages<T> {
  items: T[]
  /** True when the server still had more pages after the bound. */
  moreNotShown: boolean
}

/** Follows `nextCursor` until the list ends or `paging.maxPages` pages were read. */
export async function fetchAllPages<T>(
  fetchPage: (cursor: string | undefined) => Promise<CursorPage<T>>,
): Promise<AllPages<T>> {
  const items: T[] = []
  let cursor: string | undefined
  for (let page = 0; page < paging.maxPages; page += 1) {
    const result = await fetchPage(cursor)
    items.push(...result.items)
    cursor = result.nextCursor ?? undefined
    if (!cursor) return { items, moreNotShown: false }
  }
  return { items, moreNotShown: true }
}

/** Poll intervals in milliseconds. Background tabs are not polled (library default). */
export const POLL = { fast: 3_000, normal: 10_000, slow: 30_000 } as const

/**
 * Polling interval that stops once the server has said "not supported yet": asking again every few
 * seconds would only repeat the same answer.
 */
export const every =
  (ms: number) =>
  (query: { state: { error: unknown } }): number | false =>
    isUnsupported(query.state.error) ? false : ms

export const keys = {
  nodeState: ['nodeState'] as const,
  cluster: ['cluster'] as const,
  clusterSummary: ['clusterSummary'] as const,
  servers: ['servers'] as const,
  server: (id: string) => ['server', id] as const,
  serverGroups: ['serverGroups'] as const,
  databases: ['databases'] as const,
  createOptions: ['databaseCreateOptions'] as const,
  table: (db: string, schema: string, table: string) => ['table', db, schema, table] as const,
  database: (name: string) => ['database', name] as const,
  tables: (name: string) => ['tables', name] as const,
  colocation: (name: string) => ['colocation', name] as const,
  buckets: (name: string, colocationId: number) => ['buckets', name, colocationId] as const,
  bucketDistribution: (name: string, colocationId?: number) =>
    ['bucketDistribution', name, colocationId ?? null] as const,
  topology: ['topology'] as const,
  rebalance: ['rebalance'] as const,
  rebalanceReports: ['rebalanceReports'] as const,
  tasks: ['tasks'] as const,
  task: (id: number) => ['task', id] as const,
  roles: ['roles'] as const,
  clusterRoles: ['clusterRoles'] as const,
  grants: ['grants'] as const,
  networkAccess: ['networkAccess'] as const,
  audit: ['audit'] as const,
  clusterSettings: ['clusterSettings'] as const,
  ledgerStatus: ['ledgerStatus'] as const,
  ledger: (name: string) => ['ledger', name] as const,
  alerts: ['alerts'] as const,
  logs: ['logs'] as const,
  metrics: ['metrics'] as const,
  thisServer: ['thisServer'] as const,
} satisfies Record<string, QueryKey | ((...args: never[]) => QueryKey)>

/**
 * Creates the application query client.
 * @param onSessionLost called once for every request that fails with an expired session.
 */
export function createAppQueryClient(onSessionLost: () => void = () => {}): QueryClient {
  return new QueryClient({
    queryCache: new QueryCache({
      onError: (error) => {
        if (isSessionLost(error)) onSessionLost()
      },
    }),
    // A mutation that meets an expired session ends the session just like a query does.
    mutationCache: new MutationCache({
      onError: (error) => {
        if (isSessionLost(error)) onSessionLost()
      },
    }),
    defaultOptions: {
      queries: {
        staleTime: 5_000,
        gcTime: 5 * 60_000,
        retry: shouldRetry,
        refetchOnWindowFocus: true,
      },
      // Mutations are never retried automatically: they may not be idempotent.
      mutations: { retry: false },
    },
  })
}

// --- cluster ---------------------------------------------------------------------------------

/** `enabled` is false for accounts that may not read the cluster (kingres_stats only). */
export const useCluster = (enabled: MaybeRefOrGetter<boolean> = true) =>
  useQuery({
    queryKey: keys.cluster,
    queryFn: endpoints.getCluster,
    refetchInterval: every(POLL.fast),
    enabled: computed(() => toValue(enabled)),
  })

/** The stats-level view of the cluster (needs only kingres_stats). */
export const useClusterSummary = (enabled: MaybeRefOrGetter<boolean> = true) =>
  useQuery({
    queryKey: keys.clusterSummary,
    queryFn: endpoints.getClusterSummary,
    refetchInterval: every(POLL.fast),
    enabled: computed(() => toValue(enabled)),
  })

export const useServers = () =>
  useQuery({ queryKey: keys.servers, queryFn: endpoints.listServers, refetchInterval: POLL.fast })

export const useServer = (id: MaybeRefOrGetter<string>) =>
  useQuery({
    queryKey: computed(() => keys.server(toValue(id))),
    queryFn: () => endpoints.getServer(toValue(id)),
    refetchInterval: every(POLL.fast),
  })

export const useServerGroups = () =>
  useQuery({ queryKey: keys.serverGroups, queryFn: endpoints.listServerGroups })

export const useClusterSettings = () =>
  useQuery({ queryKey: keys.clusterSettings, queryFn: endpoints.getClusterSettings })

// --- databases and shards ---------------------------------------------------------------------

export const useDatabases = () =>
  useQuery({
    queryKey: keys.databases,
    queryFn: endpoints.listDatabases,
    refetchInterval: every(POLL.normal),
  })

export const useDatabaseCreateOptions = () =>
  useQuery({
    queryKey: keys.createOptions,
    queryFn: endpoints.getDatabaseCreateOptions,
    staleTime: 60_000,
  })

export const useTableDetail = (db: string, schema: string, table: string) =>
  useQuery({
    queryKey: keys.table(db, schema, table),
    queryFn: () => endpoints.getTable(db, schema, table),
  })

export const useDatabase = (name: MaybeRefOrGetter<string>) =>
  useQuery({
    queryKey: computed(() => keys.database(toValue(name))),
    queryFn: () => endpoints.getDatabase(toValue(name)),
    refetchInterval: every(POLL.normal),
  })

export const useTables = (name: MaybeRefOrGetter<string>) =>
  useQuery({
    queryKey: computed(() => keys.tables(toValue(name))),
    queryFn: () =>
      fetchAllPages((cursor) => endpoints.listTables(toValue(name), cursor ? { cursor } : {})),
  })

export const useColocationGroups = (name: MaybeRefOrGetter<string | null>) =>
  useQuery({
    queryKey: computed(() => keys.colocation(toValue(name) ?? '')),
    queryFn: () => endpoints.listColocationGroups(toValue(name) ?? ''),
    enabled: computed(() => toValue(name) !== null && toValue(name) !== ''),
  })

/** All buckets of one colocation group (follows cursors until the list ends). */
export const useAllBuckets = (
  name: MaybeRefOrGetter<string | null>,
  colocationId: MaybeRefOrGetter<number | null>,
) =>
  useQuery({
    queryKey: computed(() => keys.buckets(toValue(name) ?? '', toValue(colocationId) ?? -1)),
    enabled: computed(() => !!toValue(name) && toValue(colocationId) !== null),
    refetchInterval: every(POLL.normal),
    queryFn: async () => {
      const db = toValue(name) ?? ''
      const colocation = toValue(colocationId) ?? 0
      let mapVersion = 0
      const all = await fetchAllPages(async (cursor) => {
        const page = await endpoints.listBuckets(db, colocation, { cursor })
        mapVersion = page.mapVersion
        return page
      })
      return { mapVersion, ...all }
    },
  })

export const useBucketDistribution = (
  name: MaybeRefOrGetter<string | null>,
  colocationId: MaybeRefOrGetter<number | undefined>,
) =>
  useQuery({
    queryKey: computed(() => keys.bucketDistribution(toValue(name) ?? '', toValue(colocationId))),
    queryFn: () => endpoints.getBucketDistribution(toValue(name) ?? '', toValue(colocationId)),
    enabled: computed(() => !!toValue(name)),
    refetchInterval: every(POLL.normal),
  })

// --- replication and rebalance ----------------------------------------------------------------

export const useTopology = () =>
  useQuery({
    queryKey: keys.topology,
    queryFn: endpoints.getReplicationTopology,
    refetchInterval: every(POLL.fast),
  })

export const useRebalanceStatus = () =>
  useQuery({
    queryKey: keys.rebalance,
    queryFn: endpoints.getRebalanceStatus,
    refetchInterval: (q) =>
      isUnsupported(q.state.error) ? false : q.state.data?.state === 'running' ? 1_500 : POLL.fast,
  })

export const useRebalanceReports = () =>
  useQuery({
    queryKey: keys.rebalanceReports,
    queryFn: async () => {
      const page = await endpoints.listRebalanceReports({ limit: 20 })
      return { items: page.items, moreNotShown: !!page.nextCursor }
    },
  })

// --- tasks, alerts, logs ----------------------------------------------------------------------

export const useTasks = (limit = 50, enabled: MaybeRefOrGetter<boolean> = true) =>
  useQuery({
    enabled: computed(() => toValue(enabled)),
    queryKey: [...keys.tasks, limit],
    queryFn: () => endpoints.listTasks({ limit }),
    refetchInterval: every(POLL.fast),
  })

export const useAlerts = (
  state?: 'open' | 'acknowledged' | 'resolved',
  enabled: MaybeRefOrGetter<boolean> = true,
) =>
  useQuery({
    enabled: computed(() => toValue(enabled)),
    queryKey: [...keys.alerts, state ?? 'all'],
    queryFn: () => endpoints.listAlerts({ limit: 100, state }),
    refetchInterval: every(POLL.normal),
  })

export const useMetric = (metric: MetricName, rangeMinutes = 60, groupBy?: 'server' | 'database') =>
  useQuery({
    queryKey: [...keys.metrics, metric, rangeMinutes, groupBy ?? null],
    queryFn: () => {
      const to = new Date()
      const from = new Date(to.getTime() - rangeMinutes * 60_000)
      return endpoints.queryMetrics({
        metric,
        from: from.toISOString(),
        to: to.toISOString(),
        step: Math.max(15, Math.floor((rangeMinutes * 60) / 120)),
        groupBy,
      })
    },
    refetchInterval: every(POLL.normal),
    staleTime: 10_000,
  })

// --- security, ledger -------------------------------------------------------------------------

export const useRoles = (search?: MaybeRefOrGetter<string>) =>
  useQuery({
    queryKey: computed(() => [...keys.roles, toValue(search) ?? '']),
    queryFn: () =>
      fetchAllPages((cursor) =>
        endpoints.listRoles({ limit: 500, search: toValue(search) || undefined, cursor }),
      ),
  })

export const useClusterRoles = () =>
  useQuery({ queryKey: keys.clusterRoles, queryFn: endpoints.listClusterRoles, staleTime: 60_000 })

/**
 * Ledger status of every database that has a ledger. The contract has no 404 for GET /ledger/status,
 * but kingresd 0.2.0-dev answers 404 not_found for it (a known deviation, listed in the README). So
 * that the page still works against it, a 404 is answered by asking each database with a ledger for
 * its own status, which that server does implement.
 */
async function ledgerStatusList() {
  /** Databases whose ledger status could not be read; they are reported, never left out. */
  const unreadable: { databaseName: string; error: unknown }[] = []
  try {
    return { ...(await endpoints.listLedgerStatus()), unreadable }
  } catch (error) {
    if (!(error instanceof ApiError) || error.code !== 'not_found') throw error
    const databases = await endpoints.listDatabases()
    const items = []
    for (const d of databases.items) {
      if (d.ledger.mode === 'off') continue
      try {
        items.push(await endpoints.getLedgerStatus(d.name))
      } catch (e) {
        if (e instanceof ApiError && isSessionLost(e)) throw e
        unreadable.push({ databaseName: d.name, error: e })
      }
    }
    return { items, unreadable }
  }
}

export const useLedgerStatusList = () =>
  useQuery({
    queryKey: keys.ledgerStatus,
    queryFn: ledgerStatusList,
    refetchInterval: every(POLL.normal),
  })

// --- this server ------------------------------------------------------------------------------

export const useThisServer = () =>
  useQuery({
    queryKey: keys.thisServer,
    queryFn: endpoints.getThisServer,
    refetchInterval: every(POLL.normal),
  })

// --- generic cursor-paged lists ---------------------------------------------------------------

/** A cursor-paged list with "load more". `fetchPage` receives the cursor of the page to load. */
export function usePagedQuery<T>(
  key: MaybeRefOrGetter<QueryKey>,
  fetchPage: (cursor: string | undefined) => Promise<CursorPage<T>>,
  refetchInterval?: number,
  enabled: MaybeRefOrGetter<boolean> = true,
) {
  const query = useInfiniteQuery({
    queryKey: computed(() => toValue(key)),
    queryFn: ({ pageParam }) => fetchPage(pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.nextCursor ?? undefined,
    refetchInterval: refetchInterval === undefined ? undefined : every(refetchInterval),
    enabled: computed(() => toValue(enabled)),
  })
  const items = computed(() => query.data.value?.pages.flatMap((p) => p.items) ?? [])
  return { query, items }
}
