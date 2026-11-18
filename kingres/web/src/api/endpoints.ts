// SPDX-License-Identifier: AGPL-3.0-only
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
  Task,
  BucketState,
  ConfigChangeRequest,
  ConfigLevel,
  ConfigParameterKind,
  ConfigServerState,
  RestartPendingRequest,
} from './types'
import type { components } from './schema'
import { parseProof, withRawDocuments, type RawDigest, type RawProof } from '@/lib/ledger-proof'

type S = components['schemas']
const j = unwrap

/** The counterpart of `startedTask` for a request that says `dryRun`: the task cannot come back. */
async function plan(answer: Promise<Task | S['FailoverPlan']>): Promise<S['FailoverPlan']> {
  const result = await answer
  if (!('generatedAt' in result)) throw new Error('The server answered with a task, not a plan.')
  return result
}

/**
 * Since 0.10.0 a failover request answers 202 with a task, or 200 with a plan when it says `dryRun`.
 * The panel's requests never say it, so the plan cannot come back; a server that sends one anyway is
 * a server that broke the contract, and the caller gets an error instead of a plan read as a task.
 */
async function startedTask(answer: Promise<Task | S['FailoverPlan']>): Promise<Task> {
  const result = await answer
  if ('generatedAt' in result) throw new Error('The server answered with a plan, not a task.')
  return result
}

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
  /**
   * Starts a failover (202 + task). The second argument is a mode, or the whole request: the groups, the standby per
   * group and `allowDataLoss` (a repeat of the request for a paused task resumes it). It never asks for a plan.
   */
  failoverServer: (
    serverId: ServerId,
    request: 'hard' | 'graceful' | Omit<S['FailoverRequest'], 'dryRun'>,
  ) =>
    startedTask(
      j(
        api.POST('/servers/{serverId}/failover', {
          params: { path: { serverId } },
          body: typeof request === 'string' ? { mode: request } : { ...request },
        }),
      ),
    ),
  /** Gives the server a new certificate now (202 + task `certificate.renew`). */
  renewServerCertificates: (serverId: ServerId) =>
    j(
      api.POST('/servers/{serverId}/certificates/renewals', {
        params: { path: { serverId } },
      }),
    ),
  /** Revokes every certificate of the server (202 + task `certificate.revoke`). Cannot be undone. */
  revokeServerCertificates: (serverId: ServerId) =>
    j(
      api.POST('/servers/{serverId}/certificates/revocations', {
        params: { path: { serverId } },
      }),
    ),
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
  restorePreferredPrimaries: () =>
    startedTask(j(api.POST('/replication/restore-preferred-primaries'))),
  getRebalanceStatus: () => j(api.GET('/rebalance')),
  startRebalance: (body: S['RebalanceOptions']) => j(api.POST('/rebalance', { body })),
  previewRebalancePlan: (body: S['RebalancePlanRequest']) =>
    j(api.POST('/rebalance/plan', { body })),
  stopRebalance: () => j(api.POST('/rebalance/stop')),
  listRebalanceReports: (q: Page = {}) =>
    j(api.GET('/rebalance/reports', { params: { query: q } })),
  getRebalanceReport: (reportId: number) =>
    j(api.GET('/rebalance/reports/{reportId}', { params: { path: { reportId } } })),

  // --- failover (0.10.0)
  /** The plan of a failover (200, nothing changes): the request says `dryRun`. */
  previewFailover: (serverId: ServerId, body: Omit<S['FailoverRequest'], 'dryRun'>) =>
    plan(
      j(
        api.POST('/servers/{serverId}/failover', {
          params: { path: { serverId } },
          body: { ...body, dryRun: true },
        }),
      ),
    ),
  getFailoverStatus: () => j(api.GET('/failover')),
  listFailoverReports: (q: Page = {}) => j(api.GET('/failover/reports', { params: { query: q } })),
  getFailoverReport: (reportId: number) =>
    j(api.GET('/failover/reports/{reportId}', { params: { path: { reportId } } })),
  resetAutoFailoverCounter: () => j(api.POST('/settings/cluster/auto-failover/reset')),

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
  /** Replaces the rules: 202 + task (the answer of the real change). */
  replaceNetworkAccess: async (body: Omit<NetworkAccess, 'applyStatus'>): Promise<Task> =>
    (await j(api.PUT('/settings/network-access', { body }))) as Task,
  /**
   * The same request as a dry run (contract 0.6.0): validation, the revision check (409) and the lock-out
   * rule (422) as for a real change, but nothing is changed, no task is made and nothing is audited.
   * A change that would be refused is answered with a problem; the 200 answer is always `valid: true`.
   */
  checkNetworkAccess: async (
    body: Omit<NetworkAccess, 'applyStatus'>,
  ): Promise<S['NetworkAccessCheck']> =>
    (await j(
      api.PUT('/settings/network-access', { body, params: { query: { dryRun: true } } }),
    )) as S['NetworkAccessCheck'],
  listAuditEvents: (
    q: Page & { actor?: string; action?: AuditAction; outcome?: S['AuditOutcome'] } = {},
  ) => j(api.GET('/audit', { params: { query: q } })),

  // --- settings
  getClusterSettings: () => j(api.GET('/settings/cluster')),
  updateClusterSettings: (body: ClusterSettingsUpdateRequest) =>
    j(api.PATCH('/settings/cluster', { body })) as Promise<S['ClusterSettings']>,
  /**
   * 0.9.0: the same update, asking for the task it starts (`Prefer: respond-async`). The answer is the task (202)
   * when the update started one, the settings (200) when it did not.
   */
  updateClusterSettingsAsync: async (
    body: ClusterSettingsUpdateRequest,
  ): Promise<{ task: S['Task'] | null; settings: S['ClusterSettings'] | null }> => {
    const r = (await j(
      api.PATCH('/settings/cluster', { body, headers: { Prefer: 'respond-async' } }),
    )) as S['Task'] | S['ClusterSettings']
    return 'kind' in r && 'progress' in r
      ? { task: r, settings: null }
      : { task: null, settings: r as S['ClusterSettings'] }
  },

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
  /** Digest page; every chain also carries `rawDocument`, the document's exact text (see ledger-proof.ts). */
  listLedgerDigests: async (
    databaseName: string,
    q: Page = {},
  ): Promise<{ items: RawDigest[]; nextCursor: string | null }> => {
    const text = await j(
      api.GET('/databases/{databaseName}/ledger/digests', {
        params: { path: { databaseName }, query: q },
        parseAs: 'text',
      }),
    )
    const page = JSON.parse(text) as { items: S['LedgerDigest'][]; nextCursor: string | null }
    withRawDocuments(page, text)
    return page as { items: RawDigest[]; nextCursor: string | null }
  },
  generateLedgerDigest: async (databaseName: string): Promise<RawDigest> => {
    const text = await j(
      api.POST('/databases/{databaseName}/ledger/digests', {
        params: { path: { databaseName } },
        parseAs: 'text',
      }),
    )
    const digest = JSON.parse(text) as S['LedgerDigest']
    withRawDocuments(digest, text)
    return digest as RawDigest
  },
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
  getLedgerVerification: (databaseName: string, verificationId: number) =>
    j(
      api.GET('/databases/{databaseName}/ledger/verifications/{verificationId}', {
        params: { path: { databaseName, verificationId } },
      }),
    ),
  /** The proof as the server sent it: `raw` is what is saved for `kingres-verify`, unchanged. */
  createInclusionProof: async (
    databaseName: string,
    body: S['InclusionProofRequest'],
  ): Promise<RawProof> =>
    parseProof(
      await j(
        api.POST('/databases/{databaseName}/ledger/inclusion-proofs', {
          params: { path: { databaseName } },
          body,
          parseAs: 'text',
        }),
      ),
    ),

  // --- ledger of phase 7 (0.11.0): settings, signing, publication, anchoring, aggregates, bundles, client keys
  getLedgerSettings: () => j(api.GET('/ledger/settings')),
  updateLedgerSettings: (body: S['LedgerSettingsUpdate']) =>
    j(api.PUT('/ledger/settings', { body })),
  getLedgerSigning: () => j(api.GET('/ledger/signing')),
  /** 202 + task `ledger.key.create` (the first key) or `ledger.key.rotate`. */
  createLedgerSigningKey: async (body: S['CreateLedgerSigningKeyRequest'] = {}): Promise<Task> =>
    (await j(api.POST('/ledger/signing/keys', { body }))) as Task,
  /** 202 + task `ledger.key.retire`. */
  retireLedgerSigningKey: async (fingerprint: string): Promise<Task> =>
    (await j(
      api.POST('/ledger/signing/keys/{fingerprint}/retire', {
        params: { path: { fingerprint } },
        body: {},
      }),
    )) as Task,
  /** 202 + task `ledger.key.revoke`. */
  revokeLedgerSigningKey: async (
    fingerprint: string,
    body: S['RevokeLedgerSigningKeyRequest'],
  ): Promise<Task> =>
    (await j(
      api.POST('/ledger/signing/keys/{fingerprint}/revoke', {
        params: { path: { fingerprint } },
        body,
      }),
    )) as Task,
  /** The trust file exactly as the server sent it (the bytes are saved unchanged; it is a download, not something to edit). */
  getLedgerTrustFile: (): Promise<string> =>
    j(api.GET('/ledger/signing/trust-file', { parseAs: 'text' })) as Promise<string>,
  getLedgerPublication: () => j(api.GET('/ledger/publication')),
  listLedgerAnchorTargets: () => j(api.GET('/ledger/anchor-targets')),
  createLedgerAnchorTarget: (body: S['LedgerAnchorTargetCreate']) =>
    j(api.POST('/ledger/anchor-targets', { body })),
  getLedgerAnchorTarget: (targetId: number) =>
    j(api.GET('/ledger/anchor-targets/{targetId}', { params: { path: { targetId } } })),
  updateLedgerAnchorTarget: (targetId: number, body: S['LedgerAnchorTargetUpdate']) =>
    j(api.PATCH('/ledger/anchor-targets/{targetId}', { params: { path: { targetId } }, body })),
  deleteLedgerAnchorTarget: (targetId: number) =>
    j(api.DELETE('/ledger/anchor-targets/{targetId}', { params: { path: { targetId } } })),
  testLedgerAnchorTarget: (targetId: number) =>
    j(
      api.POST('/ledger/anchor-targets/{targetId}/test', {
        params: { path: { targetId } },
        body: {},
      }),
    ),
  getLedgerAggregate: (databaseName: string, size?: number) =>
    j(
      api.GET('/databases/{databaseName}/ledger/aggregate', {
        params: { path: { databaseName }, query: size === undefined ? {} : { size } },
      }),
    ),
  listLedgerAggregates: (databaseName: string, q: Page = {}) =>
    j(
      api.GET('/databases/{databaseName}/ledger/aggregates', {
        params: { path: { databaseName }, query: q },
      }),
    ),
  listLedgerAnchors: (
    databaseName: string,
    q: Page & { targetId?: number; state?: S['LedgerAnchorState'] } = {},
  ) =>
    j(
      api.GET('/databases/{databaseName}/ledger/anchors', {
        params: { path: { databaseName }, query: q },
      }),
    ),
  /** 201 with the records that were created (no task). */
  anchorLedgerNow: (databaseName: string) =>
    j(
      api.POST('/databases/{databaseName}/ledger/anchors', {
        params: { path: { databaseName } },
        body: {},
      }),
    ),
  retryLedgerAnchor: (databaseName: string, anchorId: number) =>
    j(
      api.POST('/databases/{databaseName}/ledger/anchors/{anchorId}/retry', {
        params: { path: { databaseName, anchorId } },
        body: {},
      }),
    ),
  downloadLedgerAnchor: (databaseName: string, anchorId: number, format: 'json' | 'ots' | 'tsr') =>
    j(
      api.GET('/databases/{databaseName}/ledger/anchors/{anchorId}/evidence', {
        params: { path: { databaseName, anchorId }, query: { format } },
        parseAs: 'blob',
      }),
    ) as Promise<Blob>,
  /** 202 + task `ledger.bundle.create`. */
  createLedgerBundle: async (databaseName: string, body: S['LedgerBundleRequest']): Promise<Task> =>
    (await j(
      api.POST('/databases/{databaseName}/ledger/bundles', {
        params: { path: { databaseName } },
        body,
      }),
    )) as Task,
  getLedgerBundle: (databaseName: string, bundleId: number) =>
    j(
      api.GET('/databases/{databaseName}/ledger/bundles/{bundleId}', {
        params: { path: { databaseName, bundleId } },
        parseAs: 'blob',
      }),
    ) as Promise<Blob>,
  listLedgerClientKeys: (databaseName: string) =>
    j(
      api.GET('/databases/{databaseName}/ledger/client-keys', {
        params: { path: { databaseName } },
      }),
    ),
  registerLedgerClientKey: (databaseName: string, body: S['RegisterLedgerClientKeyRequest']) =>
    j(
      api.POST('/databases/{databaseName}/ledger/client-keys', {
        params: { path: { databaseName } },
        body,
      }),
    ),
  revokeLedgerClientKey: (databaseName: string, keyId: string, reason: string) =>
    j(
      api.POST('/databases/{databaseName}/ledger/client-keys/{keyId}/revoke', {
        params: { path: { databaseName, keyId } },
        body: { reason },
      }),
    ),
  setTableSignaturePolicy: (databaseName: string, schemaName: string, tableName: string) =>
    j(
      api.PUT(
        '/databases/{databaseName}/schemas/{schemaName}/tables/{tableName}/ledger/signature-policy',
        {
          params: { path: { databaseName, schemaName, tableName } },
          body: { policy: 'required' },
        },
      ),
    ),
  /** 202 + task `ledger.upgrade`. */
  upgradeLedgerExtension: async (databaseName: string): Promise<Task> =>
    (await j(
      api.POST('/databases/{databaseName}/ledger/upgrade', {
        params: { path: { databaseName } },
        body: {},
      }),
    )) as Task,
  /** 202 + task `ledger.epoch.start`. */
  acknowledgeLedgerDivergence: async (
    databaseName: string,
    shardGroupId: number,
    body: S['AcknowledgeLedgerDivergenceRequest'],
  ): Promise<Task> =>
    (await j(
      api.POST('/databases/{databaseName}/ledger/chains/{shardGroupId}/acknowledge', {
        params: { path: { databaseName, shardGroupId } },
        body,
      }),
    )) as Task,

  // --- configuration (0.8.0)
  listConfigParameters: (
    q: Page & {
      q?: string
      category?: string
      changed?: boolean
      differing?: boolean
      pendingRestart?: boolean
      state?: ConfigServerState
      kind?: ConfigParameterKind
    } = {},
  ) => j(api.GET('/config/parameters', { params: { query: q } })),
  getConfigParameter: (name: string) =>
    j(api.GET('/config/parameters/{name}', { params: { path: { name } } })),
  /** A question, not a request: answers 200 also when the change would be refused (`valid: false`). */
  previewConfigChange: (body: ConfigChangeRequest) =>
    j(api.POST('/config/changes/preview', { body })),
  /** 202 + task `config.apply`. */
  applyConfigChange: async (body: ConfigChangeRequest): Promise<Task> =>
    (await j(api.POST('/config/changes', { body }))) as Task,
  listConfigChanges: (q: Page & { name?: string; level?: ConfigLevel; scopeId?: string } = {}) =>
    j(api.GET('/config/changes', { params: { query: q } })),
  /** 202 + task `config.apply`: a new change that puts the earlier value back. */
  revertConfigChange: async (changeId: number): Promise<Task> =>
    (await j(
      api.POST('/config/changes/{changeId}/revert', { params: { path: { changeId } } }),
    )) as Task,
  listConfigServers: () => j(api.GET('/config/servers')),
  getConfigSummary: () => j(api.GET('/config/summary')),
  getClusterTime: () => j(api.GET('/config/time')),
  listTimeZones: () => j(api.GET('/config/time-zones')),
  /** 202 + task `config.restart`. */
  restartPendingInstances: async (body: RestartPendingRequest): Promise<Task> =>
    (await j(api.POST('/config/restarts', { body }))) as Task,

  // --- workbench, alerts, logs, metrics
  executeQuery: (body: S['QueryRequest'], signal?: AbortSignal) =>
    j(api.POST('/query', { body, signal })),
  /** Cancels a statement of the signed-in role that runs now on this server (204; 404 when none has this id). */
  cancelStatement: (statementId: string) =>
    j(api.POST('/query/statements/{statementId}/cancel', { params: { path: { statementId } } })),
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
