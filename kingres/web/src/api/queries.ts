// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// Server state: TanStack Query hooks over the endpoint functions. Query keys are defined once in
// `keys` so that mutations can invalidate them precisely. Polling intervals are chosen per
// resource: fast-changing state is refreshed while the tab is visible, the rest on demand.
import { computed, ref, toValue, watch, type MaybeRefOrGetter } from 'vue'
import {
  MutationCache,
  QueryClient,
  QueryCache,
  useInfiniteQuery,
  useQuery,
  useQueryClient,
  type QueryKey,
} from '@tanstack/vue-query'
import { endpoints } from './endpoints'
import { ApiError, isRejectedCursor, isSessionLost, isUnsupported, shouldRetry } from './problem'
import type { MetricName, MissingServer, TableChangeRequest } from './types'
import { useAnnounce } from '@/composables/useAnnounce'
import { mergeCompleteness } from '@/lib/partial'

/**
 * How many pages a "load everything" query follows. A server that keeps answering with a cursor
 * must not keep the browser busy for ever; what is left over is reported (`moreNotShown`).
 */
export const paging = { maxPages: 20 }

interface CursorPage<T> {
  items: T[]
  nextCursor?: string | null
  /** Contract 0.5.0, cluster-wide lists only: some servers did not answer. */
  partial?: boolean
  missingServers?: MissingServer[]
}

export interface AllPages<T> {
  items: T[]
  /** True when the server still had more pages after the bound. */
  moreNotShown: boolean
  /** True when the server refused a cursor (400) and the list was read again from the first page. */
  restarted?: boolean
}

/**
 * Follows `nextCursor` until the list ends or `paging.maxPages` pages were read. A cursor the server
 * refuses (400: a cursor is valid only for the revision and the moment that made it) is not a dead
 * end: the list is read again from the first page, once.
 */
export async function fetchAllPages<T>(
  fetchPage: (cursor: string | undefined) => Promise<CursorPage<T>>,
): Promise<AllPages<T>> {
  let items: T[] = []
  let cursor: string | undefined
  let restarted = false
  for (let page = 0; page < paging.maxPages; page += 1) {
    let result: CursorPage<T>
    try {
      result = await fetchPage(cursor)
    } catch (error) {
      if (cursor === undefined || restarted || !isRejectedCursor(error)) throw error
      restarted = true
      items = []
      cursor = undefined
      page = -1
      continue
    }
    items.push(...result.items)
    cursor = result.nextCursor ?? undefined
    if (!cursor) return { items, moreNotShown: false, ...(restarted ? { restarted } : {}) }
  }
  return { items, moreNotShown: true, ...(restarted ? { restarted } : {}) }
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
  tablePartitions: (db: string, schema: string, table: string) =>
    ['tablePartitions', db, schema, table] as const,
  tableChange: (db: string, schema: string, table: string, request: string) =>
    ['tableChange', db, schema, table, request] as const,
  colocation: (name: string) => ['colocation', name] as const,
  buckets: (name: string, colocationId: number) => ['buckets', name, colocationId] as const,
  bucketDistribution: (name: string, colocationId?: number) =>
    ['bucketDistribution', name, colocationId ?? null] as const,
  topology: ['topology'] as const,
  rebalance: ['rebalance'] as const,
  rebalanceReports: ['rebalanceReports'] as const,
  failover: ['failover'] as const,
  failoverReport: (id: number) => ['failoverReport', id] as const,
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
  ledgerSettings: ['ledgerSettings'] as const,
  ledgerSigning: ['ledgerSigning'] as const,
  ledgerPublication: ['ledgerPublication'] as const,
  ledgerAnchorTargets: ['ledgerAnchorTargets'] as const,
  ledgerClientKeys: (name: string) => ['ledger', name, 'clientKeys'] as const,
  alerts: ['alerts'] as const,
  logs: ['logs'] as const,
  metrics: ['metrics'] as const,
  thisServer: ['thisServer'] as const,
  config: ['config'] as const,
  configServers: ['config', 'servers'] as const,
  configSummary: ['config', 'summary'] as const,
  configTime: ['config', 'time'] as const,
  configChanges: ['config', 'changes'] as const,
  timeZones: ['config', 'timeZones'] as const,
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

export const useServers = (enabled: MaybeRefOrGetter<boolean> = true) =>
  useQuery({
    queryKey: keys.servers,
    queryFn: endpoints.listServers,
    refetchInterval: POLL.fast,
    enabled: computed(() => toValue(enabled)),
  })

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

/** The table with its columns, read again while a change of it may be running. */
export const useTable = (
  db: MaybeRefOrGetter<string>,
  schema: MaybeRefOrGetter<string>,
  table: MaybeRefOrGetter<string>,
) =>
  useQuery({
    queryKey: computed(() => keys.table(toValue(db), toValue(schema), toValue(table))),
    queryFn: () => endpoints.getTable(toValue(db), toValue(schema), toValue(table)),
    refetchInterval: every(POLL.normal),
  })

/** The user partitions of a table (every page; an empty list for a table that is not user-partitioned). */
export const useTablePartitions = (
  db: MaybeRefOrGetter<string>,
  schema: MaybeRefOrGetter<string>,
  table: MaybeRefOrGetter<string>,
  enabled: MaybeRefOrGetter<boolean> = true,
) =>
  useQuery({
    queryKey: computed(() => keys.tablePartitions(toValue(db), toValue(schema), toValue(table))),
    queryFn: () =>
      fetchAllPages((cursor) =>
        endpoints.listTablePartitions(
          toValue(db),
          toValue(schema),
          toValue(table),
          cursor ? { cursor } : {},
        ),
      ),
    enabled: computed(() => toValue(enabled)),
    refetchInterval: every(POLL.normal),
  })

/**
 * What a change of a table would do (`previewTableChange`), read again whenever the request changes. It reads only; a request that is
 * not complete yet (`null`) is not sent.
 */
export const useTableChangePreview = (
  db: MaybeRefOrGetter<string>,
  schema: MaybeRefOrGetter<string>,
  table: MaybeRefOrGetter<string>,
  request: MaybeRefOrGetter<TableChangeRequest | null>,
) =>
  useQuery({
    queryKey: computed(() =>
      keys.tableChange(
        toValue(db),
        toValue(schema),
        toValue(table),
        JSON.stringify(toValue(request)),
      ),
    ),
    queryFn: () =>
      endpoints.previewTableChange(
        toValue(db),
        toValue(schema),
        toValue(table),
        toValue(request) as TableChangeRequest,
      ),
    enabled: computed(() => toValue(request) !== null),
    retry: false,
    staleTime: 0,
    gcTime: 0,
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

export const useTopology = (enabled: MaybeRefOrGetter<boolean> = true) =>
  useQuery({
    queryKey: keys.topology,
    queryFn: endpoints.getReplicationTopology,
    refetchInterval: every(POLL.fast),
    enabled: computed(() => toValue(enabled)),
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

/**
 * The failover status (contract 0.10.0), fast while a run goes on. It is read only when the role may call it
 * (kingres_stats may not) and not again when the server says it does not know the operation yet.
 */
export const useFailoverStatus = (enabled: MaybeRefOrGetter<boolean> = true) =>
  useQuery({
    queryKey: keys.failover,
    queryFn: endpoints.getFailoverStatus,
    enabled: computed(() => toValue(enabled)),
    refetchInterval: (q) =>
      isUnsupported(q.state.error) ? false : q.state.data?.state === 'running' ? 1_500 : POLL.fast,
  })

/** One failover report (a run that is paused or finished). */
export const useFailoverReport = (id: MaybeRefOrGetter<number | null>, poll = false) =>
  useQuery({
    queryKey: computed(() => keys.failoverReport(toValue(id) ?? -1)),
    queryFn: () => endpoints.getFailoverReport(toValue(id) as number),
    enabled: computed(() => toValue(id) !== null),
    refetchInterval: poll ? POLL.fast : false,
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

/** The latest task that added or removed standbys (0.7.0), newest first; one at most. */
export const useStandbyTask = () =>
  useQuery({
    queryKey: [...keys.tasks, 'standby'],
    queryFn: () => endpoints.listTasks({ kind: 'standby.reconcile', limit: 1 }),
    refetchInterval: every(POLL.fast),
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

/** 0.11.0: the settings of publication and anchoring (cluster-wide). */
export const useLedgerSettings = (enabled: MaybeRefOrGetter<boolean> = true) =>
  useQuery({
    queryKey: keys.ledgerSettings,
    queryFn: endpoints.getLedgerSettings,
    refetchInterval: every(POLL.slow),
    enabled: computed(() => toValue(enabled)),
  })

/** 0.11.0: the signing keys of the ledger, their events and the servers that hold them. Public information only. */
export const useLedgerSigning = (enabled: MaybeRefOrGetter<boolean> = true) =>
  useQuery({
    queryKey: keys.ledgerSigning,
    queryFn: endpoints.getLedgerSigning,
    refetchInterval: every(POLL.normal),
    enabled: computed(() => toValue(enabled)),
  })

/** 0.11.0: the listener and the logs of the publication (a server may answer 422 `unsupported_operation` for now). */
export const useLedgerPublication = (enabled: MaybeRefOrGetter<boolean> = true) =>
  useQuery({
    queryKey: keys.ledgerPublication,
    queryFn: endpoints.getLedgerPublication,
    refetchInterval: every(POLL.normal),
    enabled: computed(() => toValue(enabled)),
  })

/** 0.11.0: the anchor targets of the cluster (a server may answer 422 `unsupported_operation` for now). */
export const useLedgerAnchorTargets = (enabled: MaybeRefOrGetter<boolean> = true) =>
  useQuery({
    queryKey: keys.ledgerAnchorTargets,
    queryFn: endpoints.listLedgerAnchorTargets,
    refetchInterval: every(POLL.normal),
    enabled: computed(() => toValue(enabled)),
  })

/** 0.11.0: the client keys of a database (a server may answer 422 `unsupported_operation`: extension older than 0.3, or not built). */
export const useLedgerClientKeys = (name: MaybeRefOrGetter<string>) =>
  useQuery({
    queryKey: computed(() => keys.ledgerClientKeys(toValue(name))),
    queryFn: () => endpoints.listLedgerClientKeys(toValue(name)),
    refetchInterval: every(POLL.normal),
    enabled: computed(() => toValue(name) !== ''),
  })

// --- this server ------------------------------------------------------------------------------

/** The instances of this server; the shell reads it so that the host of the system database is known. */
export const useThisServerInstances = (enabled: MaybeRefOrGetter<boolean> = true) =>
  useQuery({
    queryKey: [...keys.thisServer, 'instances'],
    queryFn: endpoints.listThisServerInstances,
    refetchInterval: every(POLL.fast),
    enabled: computed(() => toValue(enabled)),
  })

export const useThisServer = () =>
  useQuery({
    queryKey: keys.thisServer,
    queryFn: endpoints.getThisServer,
    refetchInterval: every(POLL.normal),
  })

// --- generic cursor-paged lists ---------------------------------------------------------------

/** A page the list put in its cache in place of one whose cursor the server refused (see below). */
type PagedPage<T> = CursorPage<T> & { cursorRejected?: boolean }

/**
 * A cursor-paged list with "load more". `fetchPage` receives the cursor of the page to load.
 *
 * - A cursor is never stored anywhere but in this query's memory: not in the address, not in
 *   browser storage. A reload or a new session starts at the first page.
 * - A cursor the server refuses (400; since 0.5.0 an old revision's cursor does) is not an error
 *   dead end: the list starts again from the first page and `cursorReset` tells the view to say so.
 * - `completeness` says whether the latest answers were partial (contract 0.5.0).
 */
export function usePagedQuery<T>(
  key: MaybeRefOrGetter<QueryKey>,
  fetchPage: (cursor: string | undefined) => Promise<CursorPage<T>>,
  refetchInterval?: number,
  enabled: MaybeRefOrGetter<boolean> = true,
) {
  const client = useQueryClient()
  const announce = useAnnounce()
  const query = useInfiniteQuery({
    queryKey: computed(() => toValue(key)),
    queryFn: async ({ pageParam }): Promise<PagedPage<T>> => {
      try {
        return await fetchPage(pageParam)
      } catch (error) {
        if (pageParam !== undefined && isRejectedCursor(error))
          return { items: [], nextCursor: null, cursorRejected: true }
        throw error
      }
    },
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.nextCursor ?? undefined,
    refetchInterval: refetchInterval === undefined ? undefined : every(refetchInterval),
    enabled: computed(() => toValue(enabled)),
  })
  const items = computed(() => query.data.value?.pages.flatMap((p) => p.items) ?? [])
  const completeness = computed(() => mergeCompleteness(query.data.value?.pages ?? []))

  /** True after a refused cursor sent the list back to its first page. */
  const cursorReset = ref(false)
  watch(
    () => query.data.value?.pages.some((p) => p.cursorRejected) ?? false,
    (rejected) => {
      if (!rejected) return
      cursorReset.value = true
      announce('The list started again from the first page.')
      void client.resetQueries({ queryKey: toValue(key), exact: true })
    },
  )
  // Another list (a changed filter) is a new start anyway.
  watch(
    () => toValue(key),
    () => {
      cursorReset.value = false
    },
    { deep: true },
  )
  return {
    query,
    items,
    completeness,
    cursorReset,
    dismissCursorReset: () => {
      cursorReset.value = false
    },
  }
}

// --- configuration (0.8.0) ----------------------------------------------------------------------------

export const useConfigServers = (enabled: MaybeRefOrGetter<boolean> = true) =>
  useQuery({
    queryKey: keys.configServers,
    queryFn: endpoints.listConfigServers,
    refetchInterval: every(POLL.fast),
    enabled: computed(() => toValue(enabled)),
  })

/** The numbers of the dashboard card. `enabled` is false for accounts that may not read them. */
export const useConfigSummary = (enabled: MaybeRefOrGetter<boolean> = true) =>
  useQuery({
    queryKey: keys.configSummary,
    queryFn: endpoints.getConfigSummary,
    refetchInterval: every(POLL.normal),
    enabled: computed(() => toValue(enabled)),
  })

/** Clocks and zones. The Time tab refreshes every 10 seconds and shows when the last measurement was made. */
export const useClusterTime = (enabled: MaybeRefOrGetter<boolean> = true) =>
  useQuery({
    queryKey: keys.configTime,
    queryFn: endpoints.getClusterTime,
    refetchInterval: every(POLL.normal),
    enabled: computed(() => toValue(enabled)),
  })

/** About 600 names, not paginated; the list changes only with the PostgreSQL build. */
export const useTimeZones = (enabled: MaybeRefOrGetter<boolean> = true) =>
  useQuery({
    queryKey: keys.timeZones,
    queryFn: endpoints.listTimeZones,
    staleTime: 600_000,
    enabled: computed(() => toValue(enabled)),
  })

export interface ConfigFilters {
  q: string
  category: string
  changed: boolean
  differing: boolean
  pendingRestart: boolean
  state: string
  kind: string
}

/** The parameters, filtered by the server and paged by cursor ("Load more"). */
export const useConfigParameters = (filters: MaybeRefOrGetter<ConfigFilters>) =>
  usePagedQuery(
    () => [...keys.config, 'parameters', toValue(filters)],
    (cursor) => {
      const f = toValue(filters)
      return endpoints.listConfigParameters({
        limit: 50,
        cursor,
        ...(f.q ? { q: f.q } : {}),
        ...(f.category ? { category: f.category } : {}),
        ...(f.changed ? { changed: true } : {}),
        ...(f.differing ? { differing: true } : {}),
        ...(f.pendingRestart ? { pendingRestart: true } : {}),
        ...(f.state ? { state: f.state as 'failed' } : {}),
        ...(f.kind ? { kind: f.kind as 'settable' } : {}),
      })
    },
    POLL.normal,
  )

/** The history of changes, newest first, optionally of one parameter. */
export const useConfigChanges = (name: MaybeRefOrGetter<string>) =>
  usePagedQuery(
    () => [...keys.configChanges, toValue(name)],
    (cursor) =>
      endpoints.listConfigChanges({
        limit: 25,
        cursor,
        ...(toValue(name) ? { name: toValue(name) } : {}),
      }),
    POLL.normal,
  )
