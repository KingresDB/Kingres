// SPDX-License-Identifier: PostgreSQL
// Copyright (c) 2026, RooyeKhat Media Co
//
// One function per REST operation the panel calls. This is the only module (besides the
// client) that touches `api`; components and composables use these functions or the hooks in
// queries.ts. Names follow the operationId in the contract.
import { api, unwrap } from './client'
import type {
  ClusterSettingsUpdateRequest,
  DatabaseCreateRequest,
  DatabaseUpdateRequest,
  GrantRequest,
  InitClusterRequest,
  JoinClusterRequest,
  LogComponent,
  LogLevel,
  MetricName,
  NetworkAccess,
  RoleCreateRequest,
  RoleUpdateRequest,
  ClusterRoleName,
  AlertSeverity,
  AuditAction,
  ServerId,
  TaskKind,
  TaskState,
  BucketState,
} from './types'
import type { components } from './schema'

type S = components['schemas']
const j = unwrap

export interface Page {
  limit?: number
  cursor?: string
}

export const endpoints = {
  // --- auth and setup
  login: (body: S['LoginRequest']) => j(api.POST('/auth/login', { body })),
  logout: () => j(api.POST('/auth/logout')),
  getSession: () => j(api.GET('/auth/session')),
  getNodeState: () => j(api.GET('/setup/state')),
  checkSetupToken: () => j(api.POST('/setup/token-check')),
  initCluster: (body: InitClusterRequest) => j(api.POST('/setup/init-cluster', { body })),
  joinCluster: (body: JoinClusterRequest) => j(api.POST('/setup/join-cluster', { body })),

  // --- this server
  getThisServer: () => j(api.GET('/this-server')),
  getThisServerResources: () => j(api.GET('/this-server/resources')),
  listThisServerInstances: () => j(api.GET('/this-server/instances')),
  instanceAction: (id: number, action: 'restart' | 'stop' | 'start', force = false) => {
    const params = { path: { instanceId: id } }
    if (action === 'start')
      return j(api.POST('/this-server/instances/{instanceId}/start', { params }))
    const path =
      action === 'stop'
        ? '/this-server/instances/{instanceId}/stop'
        : '/this-server/instances/{instanceId}/restart'
    return j(api.POST(path, { params, body: { force } }))
  },
  getThisServerSettings: () => j(api.GET('/this-server/settings')),
  updateThisServerSettings: (body: S['LocalSettingsUpdate']) =>
    j(api.PATCH('/this-server/settings', { body })),
  leaveCluster: (force: boolean) => j(api.POST('/this-server/leave-cluster', { body: { force } })),
  listThisServerLogs: (q: Page & { level?: LogLevel; component?: LogComponent; q?: string }) =>
    j(api.GET('/this-server/logs', { params: { query: q } })),

  // --- cluster and servers
  getCluster: () => j(api.GET('/cluster')),
  getClusterSummary: () => j(api.GET('/cluster/summary')),
  enableTableLedger: (
    databaseName: string,
    schemaName: string,
    tableName: string,
    mode: 'appendOnly' | 'updatable',
  ) =>
    j(
      api.PUT('/databases/{databaseName}/schemas/{schemaName}/tables/{tableName}/ledger', {
        params: { path: { databaseName, schemaName, tableName } },
        body: { mode },
      }),
    ),
  listServers: () => j(api.GET('/servers')),
  getServer: (serverId: ServerId) =>
    j(api.GET('/servers/{serverId}', { params: { path: { serverId } } })),
  addServer: (body: S['AddServerRequest']) => j(api.POST('/servers', { body })),
  updateServer: (serverId: ServerId, body: S['ServerUpdateRequest']) =>
    j(api.PATCH('/servers/{serverId}', { params: { path: { serverId } }, body })),
  removeServer: (serverId: ServerId) =>
    j(api.DELETE('/servers/{serverId}', { params: { path: { serverId } } })),
  failoverServer: (serverId: ServerId, mode: 'hard' | 'graceful') =>
    j(api.POST('/servers/{serverId}/failover', { params: { path: { serverId } }, body: { mode } })),
  listServerGroups: () => j(api.GET('/server-groups')),

  // --- databases
  listDatabases: () => j(api.GET('/databases')),
  getDatabase: (databaseName: string) =>
    j(api.GET('/databases/{databaseName}', { params: { path: { databaseName } } })),
  getDatabaseCreateOptions: () => j(api.GET('/database-create-options')),
  getTable: (databaseName: string, schemaName: string, tableName: string) =>
    j(
      api.GET('/databases/{databaseName}/schemas/{schemaName}/tables/{tableName}', {
        params: { path: { databaseName, schemaName, tableName } },
      }),
    ),
  createServerGroup: (body: S['ServerGroupCreateRequest']) =>
    j(api.POST('/server-groups', { body })),
  deleteServerGroup: (serverGroupName: string) =>
    j(
      api.DELETE('/server-groups/{serverGroupName}', {
        params: { path: { serverGroupName } },
      }),
    ),
  createDatabase: (body: DatabaseCreateRequest) => j(api.POST('/databases', { body })),
  updateDatabase: (databaseName: string, body: DatabaseUpdateRequest) =>
    j(api.PATCH('/databases/{databaseName}', { params: { path: { databaseName } }, body })),
  dropDatabase: (databaseName: string) =>
    j(
      api.DELETE('/databases/{databaseName}', {
        params: { path: { databaseName }, query: { confirm: databaseName } },
      }),
    ),
  listTables: (databaseName: string, q: Page = {}) =>
    j(
      api.GET('/databases/{databaseName}/tables', {
        params: { path: { databaseName }, query: { limit: 500, ...q } },
      }),
    ),
  distributeTable: (
    databaseName: string,
    schemaName: string,
    tableName: string,
    body: S['DistributeTableRequest'],
  ) =>
    j(
      api.PUT('/databases/{databaseName}/schemas/{schemaName}/tables/{tableName}/distribution', {
        params: { path: { databaseName, schemaName, tableName } },
        body,
      }),
    ),
  listColocationGroups: (databaseName: string) =>
    j(
      api.GET('/databases/{databaseName}/colocation-groups', {
        params: { path: { databaseName } },
      }),
    ),
  listBuckets: (
    databaseName: string,
    colocationId: number,
    q: Page & { shardGroupId?: number; serverId?: ServerId; state?: BucketState } = {},
  ) =>
    j(
      api.GET('/databases/{databaseName}/colocation-groups/{colocationId}/buckets', {
        params: { path: { databaseName, colocationId }, query: { limit: 500, ...q } },
      }),
    ),
  getBucketDistribution: (databaseName: string, colocationId?: number) =>
    j(
      api.GET('/databases/{databaseName}/bucket-distribution', {
        params: { path: { databaseName }, query: { colocationId } },
      }),
    ),

  // --- replication and rebalance
  getReplicationTopology: () => j(api.GET('/replication/topology')),
  restorePreferredPrimaries: () => j(api.POST('/replication/restore-preferred-primaries')),
  getRebalanceStatus: () => j(api.GET('/rebalance')),
  startRebalance: (body: S['RebalanceOptions']) => j(api.POST('/rebalance', { body })),
  previewRebalancePlan: (body: S['RebalancePlanRequest']) =>
    j(api.POST('/rebalance/plan', { body })),
  stopRebalance: () => j(api.POST('/rebalance/stop')),
  listRebalanceReports: (q: Page = {}) =>
    j(api.GET('/rebalance/reports', { params: { query: q } })),
  getRebalanceReport: (reportId: number) =>
    j(api.GET('/rebalance/reports/{reportId}', { params: { path: { reportId } } })),

  // --- tasks
  listTasks: (q: Page & { state?: TaskState; kind?: TaskKind } = {}) =>
    j(api.GET('/tasks', { params: { query: q } })),
  getTask: (taskId: number) => j(api.GET('/tasks/{taskId}', { params: { path: { taskId } } })),
  cancelTask: (taskId: number) =>
    j(api.POST('/tasks/{taskId}/cancel', { params: { path: { taskId } } })),

  // --- security
  listClusterRoles: () => j(api.GET('/cluster-roles')),
  listRoles: (q: Page & { search?: string; canLogin?: boolean } = {}) =>
    j(api.GET('/roles', { params: { query: q } })),
  createRole: (body: RoleCreateRequest) => j(api.POST('/roles', { body })),
  updateRole: (roleName: string, body: RoleUpdateRequest) =>
    j(api.PATCH('/roles/{roleName}', { params: { path: { roleName } }, body })),
  deleteRole: (roleName: string) =>
    j(api.DELETE('/roles/{roleName}', { params: { path: { roleName } } })),
  setRoleMemberships: (roleName: string, memberOf: string[]) =>
    j(
      api.PUT('/roles/{roleName}/memberships', {
        params: { path: { roleName } },
        body: { memberOf },
      }),
    ),
  setRoleClusterRoles: (roleName: string, clusterRoles: ClusterRoleName[]) =>
    j(
      api.PUT('/roles/{roleName}/cluster-roles', {
        params: { path: { roleName } },
        body: { clusterRoles },
      }),
    ),
  listGrants: (q: Page & { grantee?: string; database?: string } = {}) =>
    j(api.GET('/grants', { params: { query: { limit: 500, ...q } } })),
  createGrant: (body: GrantRequest) => j(api.POST('/grants', { body })),
  revokeGrants: (body: GrantRequest) => j(api.POST('/grants/revocations', { body })),
  applyGrantTemplate: (body: S['GrantTemplateRequest']) =>
    j(api.POST('/grants/template-applications', { body })),
  getNetworkAccess: () => j(api.GET('/settings/network-access')),
  replaceNetworkAccess: (body: Omit<NetworkAccess, 'applyStatus'>) =>
    j(api.PUT('/settings/network-access', { body })),
  listAuditEvents: (
    q: Page & { actor?: string; action?: AuditAction; outcome?: S['AuditOutcome'] } = {},
  ) => j(api.GET('/audit', { params: { query: q } })),

  // --- settings
  getClusterSettings: () => j(api.GET('/settings/cluster')),
  updateClusterSettings: (body: ClusterSettingsUpdateRequest) =>
    j(api.PATCH('/settings/cluster', { body })),

  // --- ledger
  listLedgerStatus: () => j(api.GET('/ledger/status')),
  getLedgerStatus: (databaseName: string) =>
    j(api.GET('/databases/{databaseName}/ledger', { params: { path: { databaseName } } })),
  listLedgerBlocks: (databaseName: string, q: Page & { shardGroupId?: number } = {}) =>
    j(
      api.GET('/databases/{databaseName}/ledger/blocks', {
        params: { path: { databaseName }, query: q },
      }),
    ),
  listLedgerDigests: (databaseName: string, q: Page = {}) =>
    j(
      api.GET('/databases/{databaseName}/ledger/digests', {
        params: { path: { databaseName }, query: q },
      }),
    ),
  generateLedgerDigest: (databaseName: string) =>
    j(
      api.POST('/databases/{databaseName}/ledger/digests', {
        params: { path: { databaseName } },
      }),
    ),
  listLedgerVerifications: (databaseName: string, q: Page = {}) =>
    j(
      api.GET('/databases/{databaseName}/ledger/verifications', {
        params: { path: { databaseName }, query: q },
      }),
    ),
  startLedgerVerification: (databaseName: string, body: S['LedgerVerifyRequest']) =>
    j(
      api.POST('/databases/{databaseName}/ledger/verifications', {
        params: { path: { databaseName } },
        body,
      }),
    ),
  createInclusionProof: (databaseName: string, body: S['InclusionProofRequest']) =>
    j(
      api.POST('/databases/{databaseName}/ledger/inclusion-proofs', {
        params: { path: { databaseName } },
        body,
      }),
    ),

  // --- workbench, alerts, logs, metrics
  executeQuery: (body: S['QueryRequest'], signal?: AbortSignal) =>
    j(api.POST('/query', { body, signal })),
  listAlerts: (q: Page & { state?: S['AlertState']; severity?: AlertSeverity } = {}) =>
    j(api.GET('/alerts', { params: { query: q } })),
  acknowledgeAlert: (alertId: number) =>
    j(api.PUT('/alerts/{alertId}/acknowledgement', { params: { path: { alertId } } })),
  listLogs: (
    q: Page & { level?: LogLevel; component?: LogComponent; q?: string; serverId?: ServerId },
  ) => j(api.GET('/logs', { params: { query: q } })),
  queryMetrics: (q: {
    metric: MetricName
    from: string
    to: string
    step: number
    serverId?: ServerId
    databaseName?: string
    groupBy?: 'server' | 'database'
  }) => j(api.GET('/metrics/query', { params: { query: q } })),
} as const
