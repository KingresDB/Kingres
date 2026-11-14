# KingresDB management REST API

`openapi.yaml` (OpenAPI 3.1, version 0.4.0) is the contract between the Go backend (`kingresd`) and the
Vue/TypeScript panel and `kingres-cli`. Lint it and run the mechanical rule check with:

```
npx --yes @redocly/cli@latest lint kingres/api/openapi.yaml     # from the repository root

cd kingres/api && npm install                                  # once: installs the `yaml` parser
node scripts/check-contract.mjs                                  # or: npm run check
```

`scripts/check-contract.mjs` exits 0 when every uniform rule holds and 1 otherwise (each violation is printed). It
enforces: every operation has `operationId`, `summary`, `description`, `tags` and a valid `x-required-role`; every
authenticated operation lists 401, 403, 422 and 503; every operation with a request body or query parameters lists
400; every unauthenticated or setup-token operation lists 429; every operation lists 405; every error response is
`application/problem+json` with `Problem`; every 202 has a `Location` header and a `Task` body; no schema reachable
from a request body has a `readOnly` property that is `required`; every `$ref` resolves; server ids are strings
(`ServerId`); `AuditAction` consists of operation ids plus the closed list of non-operation events and covers every
mutating operation; the default bucket count is 128 (1 to 4096); the durability levels map to
`synchronous_commit` `local`, `on`, `remote_apply`. Run it after every change to the contract and in CI.

## Conventions

- **Base path** `/api/v1`. Plural nouns, kebab-case path segments, camelCase JSON properties. Enum values are
  camelCase (`syncApply`, `pendingRebalance`), except PostgreSQL-derived names (`kingres_admin`,
  `scram-sha-256`) and dotted task kinds (`database.create`).
- **Identifiers.** They map to the catalog table `kingres.node` (one row per PostgreSQL instance with `node_id`,
  `group_id`, `host`, `port`, `role`) without arithmetic:

  | Thing | Id | Type | Source |
  |---|---|---|---|
  | Instance | `id` / `instanceId` | integer | `node_id` |
  | Shard group | `id` / `shardGroupId` | integer | `group_id`; **the map group is group 0** (`isMapGroup: true`) |
  | Server | `id` / `serverId` | **string** (`ServerId`) | the distinct `host` value of its instances |
  | Colocation group, task, report, verification, alert | integer | | their own tables |

  A server is a machine, defined as one distinct `host` in `kingres.node`; the catalog has no other notion of a
  server. Its id is that host string exactly as stored (a DNS name or an IP address, IPv6 without brackets). The id
  is opaque: clients compare it for equality and never parse or sort it numerically; it is percent-encoded in URL
  paths. It is a string so that the backend can later register server ids without changing the contract.
  Databases, roles, schemas, tables and server groups are addressed by name. Tables are
  `/databases/{databaseName}/schemas/{schemaName}/tables/{tableName}`.
- **Vocabulary.** *server* = a machine (one host); *instance* = one PostgreSQL instance on it (`kingres.node` row);
  *shard group* = one primary plus its standbys (the map group is the shard group with `group_id` 0 and
  `isMapGroup: true`); *server group* = rack or zone; *bucket* = one hash bucket of a colocation group.
- **Lists** are always an object with `items`. Large lists (tasks, audit, alerts, logs, buckets, tables, roles,
  grants, ledger blocks/digests/verifications, rebalance reports) take `limit` (1 to 500, default 50) and `cursor`
  and return `nextCursor` (null on the last page). The cursor is opaque. Small lists (servers, databases, server
  groups, colocation groups, instances) return everything.
- **Timestamps** are RFC 3339 UTC (`date-time`). Sizes are bytes (`int64`). Hashes are 64 lower-case hex characters.
  Absent optional data is returned as `null` when the property is listed in `required`.
- **`null` means "not available".** Where the backend may legitimately not know a value yet (resource usage,
  uptime, join time, bucket and table sizes, per-database settings and statistics, ledger digest and verification
  details), the response property is nullable and `null` means "not available". The server never invents a zero
  for an unknown value; clients render `null` as "n/a" and never as 0. The changelog lists the nullable
  properties.
- **Requests and responses use separate schemas** wherever a field exists in only one direction (see the
  `...CreateSettings` schemas). A request schema never contains a response-only field, so generated request types
  need nothing the client cannot know.
- **Updates**: `PATCH` is a partial update with `application/json`; nested objects are merged. `PUT` on a
  sub-resource replaces it (`memberships`, `cluster-roles`, `network-access`).
- **Request id.** Servers return `X-Request-Id`; it is copied into `Problem.requestId` and into audit events.

## Errors

All errors are RFC 9457 `application/problem+json` (`Problem`): `type`, `title`, `status`, `detail`, `instance`
plus the machine-readable `code` (enum `ProblemCode`). Clients must branch on `code`. Extensions: `errors[]`
(field-level, for `validation_failed`), `sqlState`/`sqlPosition` (for `sql_error`), `requestId`.

| Status | Meaning | Typical codes |
|---|---|---|
| 400 | Malformed request or schema violation | `bad_request`, `validation_failed` |
| 401 | Not signed in, bad credentials, bad setup token | `unauthenticated`, `invalid_credentials`, `session_expired`, `setup_token_invalid` |
| 403 | Missing cluster role, bad CSRF token, setup call from a non-permitted address | `forbidden`, `csrf_token_invalid` |
| 404 | Unknown resource | `not_found` |
| 405 | Known path, wrong method (transport level, `Allow` header) | `method_not_allowed` |
| 409 | State conflict | `already_exists`, `immutable_field`, `rebalance_in_progress`, `operation_in_progress`, `task_not_cancellable`, `last_admin`, `role_in_use`, `optimistic_lock_failed`, `no_replica_available` |
| 422 | Well-formed but cannot be done | `unprocessable`, `sql_error`, `unsupported_operation`, `node_state_invalid` |
| 429 | Rate limit on unauthenticated endpoints | `rate_limited` |
| 503 | Cannot serve now | `node_uninitialized`, `map_group_unavailable`, `leader_unavailable`, `quorum_unavailable`, `service_unavailable` |

Unexpected failures return 500 with `code: internal_error` (not listed per operation).

### Shared responses (uniform rule)

Every operation carries the same set of shared responses, so that no client or server has to guess which operation
can answer what. The rule is enforced by `scripts/check-contract.mjs`.

| Response | Carried by | Meaning |
|---|---|---|
| 401 `Unauthorized` | every authenticated operation | not signed in, session expired, bad setup token |
| 403 `Forbidden` | every authenticated operation | missing cluster role (`x-required-role`) or CSRF token; for setup-token operations: caller address not permitted while `localhostOnly` |
| 422 `UnprocessableEntity` | every authenticated operation | semantic problem, `node_state_invalid`, `sql_error`, and **`unsupported_operation`: this server version does not support the operation yet** |
| 503 `ServiceUnavailable` | every operation | node `uninitialized` (all operations except health, `/setup/*`), map group failing over, leader or quorum unavailable, shutting down |
| 400 `BadRequest` | every operation that has a request body or query parameters | invalid JSON, wrong type, unknown or out-of-range parameter |
| 429 `TooManyRequests` | every unauthenticated or setup-token operation | rate limit (`getHealth`, `getReadiness`, `getNodeState`, `login`, `checkSetupToken`, `initCluster`, `joinCluster`) |
| 405 `MethodNotAllowed` | every operation | see "Transport-level responses" |

"Authenticated" means the operation's effective security is not `security: []`. The only exceptions to the
authenticated rule are the four operations with `security: []`: `getHealth`, `getReadiness`, `login` and
`getNodeState`. They list 429 and 503 instead of 401/403/422 (`login` additionally lists its own 401 for bad
credentials and 403 for a role without a cluster role). The setup-token operations (`checkSetupToken`,
`initCluster`, `joinCluster`) are authenticated by the setup token: they list 401, 403, 422, 429 and 503.
Operation-specific responses (404, 409, and so on) are listed where they can occur.

`unsupported_operation` is answered after authentication, CSRF and role checks have passed. A server that does not
implement an operation yet answers 422 with this code for every authenticated operation; clients treat it as
"not available in this version" and not as a user error.

### Transport-level responses

Some responses are produced by the HTTP layer before any operation runs and apply to every path:

- **405 Method Not Allowed**: the path exists but not for this method (for example `DELETE /servers`). The response
  has an `Allow` header listing the supported methods and an `application/problem+json` body with
  `code: method_not_allowed`. It is defined once (`components/responses/MethodNotAllowed`); each operation
  references it only so that generated clients know about it. It is answered before authentication.
- **404** for an unknown path (`code: not_found`) and **500** (`code: internal_error`) are likewise not specific to
  an operation.

## Long-running operations and tasks

Operations that cannot finish within a request (create/drop database, distribute table, add/remove server,
failover, rebalance start, restore preferred primaries, ledger verify, instance start/stop/restart, leave cluster,
init/join cluster, apply network access) return `202 Accepted`, a `Task` body and
`Location: /api/v1/tasks/{taskId}`. Poll `GET /tasks/{taskId}`: `state` is `pending`, `running`, `paused`, `failed`
or `done`; `progress.percent` and `progress.steps[]` give detail; `error` is set when failed; `subject` names the
resource. Tasks are durable and idempotent; a conflicting second request gets 409 `operation_in_progress`.
`POST /tasks/{taskId}/cancel` requests cancellation. A stopped rebalance is `paused` and is resumed by
`POST /rebalance`. The exception is the setup operations, which are followed through `GET /setup/state` (see "Setup
flow").

### Task kinds and subjects

Each task kind has exactly one `subject.type`; `subject.id` is always a string.

| `kind` | `subject.type` | `subject.id` |
|---|---|---|
| `cluster.init`, `cluster.join` | `cluster` | `cluster` |
| `server.add`, `server.remove`, `server.failover`, `server.leave` | `server` | the `ServerId` (for `server.add` and `server.remove` the id the server registers under, which is the host of its address; for `server.leave` this server) |
| `instance.start`, `instance.stop`, `instance.restart` | `instance` | the instance id (`node_id`) as a decimal string |
| `database.create`, `database.update`, `database.drop` | `database` | the database name |
| `table.distribute` | `table` | `database/schema.table` |
| `rebalance` | `rebalance` | the task id as a decimal string (equals the report id) |
| `replication.restorePreferredPrimaries` | `replication` | `preferred-primaries` |
| `ledger.verify` | `ledgerVerification` | the **database name**: the verification id exists only after the run (see `LedgerVerification.taskId`) |
| `replicate.apply` | `replication` | `apply:<operation>:<role or database names>:<unix nanoseconds>`: the task that brings the servers that could not be reached up to date after a role or grant change (one step per server, `TaskStep.detail` names the server it waits for) |
| `networkAccess.apply` | `networkAccess` | `network-access` |

`Task.subject` is null only for tasks whose subject is not known (system tasks such as auto-failover use
`server.failover` with the failed server as subject and `createdBy: system`).

## Authentication and roles

- `POST /auth/login` with a PostgreSQL role and password sets the `kingres_session` cookie (security scheme
  `sessionCookie`) and returns the `Session`, including a `csrfToken`. Browsers send it as `X-CSRF-Token` on
  POST/PUT/PATCH/DELETE. With `issueToken: true` the response also holds a bearer `token` (scheme `bearerAuth`) for
  the CLI; bearer requests need no CSRF header. `POST /auth/logout` ends the session; `GET /auth/session` returns it.
- **The CSRF token is fixed per session.** `login` issues it; `GET /auth/session` returns the current token and
  never rotates it (rotation would break parallel requests). A new token exists only after a new `login`. For
  bearer authentication `csrfToken` is null.
- **The session carries the cluster identity.** `Session.clusterId` and `Session.clusterName` (null only when the
  server is not part of a cluster) are returned by `login` and `GET /auth/session` for every role, so an account
  that only has `kingres_stats` can render the application shell.
- The setup endpoints use the `setupToken` scheme (`X-Setup-Token` header). While a node is `uninitialized`, only
  `/healthz`, `/readyz` and `/setup/*` work; everything else returns 503 `node_uninitialized`.
- Every operation has `x-required-role`: `none`, `kingres_stats`, `kingres_readonly`, `kingres_ops` or
  `kingres_admin`. Hierarchy: admin includes ops, ops includes readonly, readonly includes stats (metrics and
  session). `kingres_stats` alone only reaches `/metrics/query` and `/auth/*`. The backend enforces the minimum
  role; the query workbench additionally runs as the caller's PostgreSQL role, so PostgreSQL privileges apply.
- A mutating cluster operation received by a server that is not the manager leader is forwarded to the leader;
  the client sees the leader's response. If the leader cannot be reached: 503 `leader_unavailable`.

## Setup flow

The wizard needs no session and never calls `GET /tasks/{taskId}` before sign-in (that operation needs a session;
the setup token is consumed by a successful init or join, so the final poll could otherwise be 401).

1. `GET /setup/state` (no authentication). `state: uninitialized` shows the wizard; `standalone`/`clustered` shows
   the login page.
2. Optional: `POST /setup/token-check` with `X-Setup-Token` returns 204 for a valid token and 401
   (`setup_token_invalid`) otherwise, without consuming the token and without a body, so the wizard can reject a
   wrong token before the user fills in the form. Rate limited (429).
3. `POST /setup/init-cluster` or `POST /setup/join-cluster` with the setup token returns 202 with a `Task` and a
   `Location`. The wizard ignores both and polls only `GET /setup/state`.
4. `NodeStateInfo.setupTask` is null until a setup request was accepted. From acceptance on it is
   `{ id, kind, state, progressPercent, error }` (kind `cluster.init` or `cluster.join`, state as for tasks, error
   as `TaskError` or null). It stays available after the task finished and after the node left `uninitialized`.
5. Outcomes: `setupTask.state` = `done` and `state` = `standalone` or `clustered`: setup finished, show the login
   page (the token is consumed, `setupTokenRequired` is false). `failed`: the node is still `uninitialized`, the
   token is still valid and `setupTask.error` explains why; the wizard can submit again (a second submit while the
   task runs is 409 `operation_in_progress`).
6. After sign-in the same task is readable with `GET /tasks/{id}` (`setupTask.id`).

## Audit actions

`AuditEvent.action` is the `operationId` of the audited operation (`createDatabase`, `dropDatabase`, `login`,
`executeQuery`, ...), or one of three events that are not an operation: `loginFailed` (a rejected sign-in, also
for an unknown user), `setupTokenRejected` and `autoFailover` (actor `system`). A denied or failed call of an
operation is recorded under that operation's id with `outcome` `denied` or `failure`. Read-only operations are not
audited except `executeQuery`. The list is the closed enum `AuditAction`, which is also the type of the `action`
filter of `GET /audit`.

## Design notes and assumptions

Where the architecture document leaves something open, the simplest option was chosen:

1. **Endpoint names** were redesigned to be resource-oriented: `POST /servers` (add), `DELETE /servers/{id}`
   (remove/drain), `POST /servers/{id}/failover`, `POST /replication/restore-preferred-primaries`,
   `GET/POST /rebalance`, `POST /rebalance/plan`, `POST /rebalance/stop`, grants as `GET/POST /grants` plus
   `POST /grants/revocations` and `POST /grants/template-applications` (a DELETE with a body was avoided),
   settings under `/settings/cluster` and `/settings/network-access`.
2. **Replica count is a cluster setting** (open decision 6). It is writable only through `PATCH /settings/cluster`
   (`defaultReplicaCount`, 0 to 3). `Database.replication.replicaCount` exists only in the response schema
   `ReplicationSettings` (the cluster's current value); the request schemas `ReplicationCreateSettings` and
   `DatabaseUpdateRequest.replication` cannot contain it. The only per-database replication setting is
   `durability`, which is mutable. The levels map to PostgreSQL `synchronous_commit` as follows:

   | `durability` | `synchronous_commit` | A commit waits for |
   |---|---|---|
   | `async` | `local` | the local flush only |
   | `sync` (default) | `on` | the flush on a quorum of synchronous standbys |
   | `syncApply` | `remote_apply` | those standbys to have applied the commit |
3. **Bucket count** defaults to 128, range 1 to 4096, and is immutable after creation (`GET
   /database-create-options` returns the default and the range for the dialog). Other immutable database fields:
   `name`, `encoding`, `lcCollate`, `lcCtype`. Mutable: owner, connection limit, durability, ledger mode and digest
   interval, and sharding from off to on.
4. **One-way switches.** Sharding on a database, the ledger mode (back to `off`), and the cluster feature flags
   can be turned on but never off; a ledger table cannot lose its ledger. `PATCH /databases/{name}` returns 200
   for synchronous changes and 202 + Task when background work is needed (enabling sharding or the ledger).
5. **Table classes.** A table has `classes` (`plain|distributed|reference|ledger`), a `distribution` object
   (null for plain) and a `ledger` object (null if not a ledger table). A table that is distributed and ledger has
   `classes: [distributed, ledger]`; `plain` never combines with the others. Distributing uses
   `PUT .../tables/{table}/distribution`. The table detail (`GET .../tables/{table}`) adds `columns[]` with
   `name`, `type`, `nullable`, `canBeDistributionColumn` and, when false, `distributionBlockedReason`
   (`shardingDisabled`, `tableNotPlain`, `unsupportedType`, `notInUniqueKey`, first applicable wins), so the panel
   can offer only valid distribution columns.
6. **Ledger per shard group.** A sharded database has one chain per shard group; status and blocks are per chain
   (`chains[]`, `shardGroupId`), and a digest contains every chain head plus an `aggregateHash` for sharded
   databases. Digest generation is synchronous (201); verification is a task and also a resource
   (`/ledger/verifications/{id}`). Digest anchoring targets are modeled (`anchors[]`) but empty until anchoring
   exists (phase 7). Generating digests and verifying need `kingres_ops`; no operation can turn the ledger off.
7. **Server groups** are named racks/zones, created and deleted via `/server-groups`; membership is set with
   `PATCH /servers/{id}` (`serverGroup`). Deleting a non-empty group is 409.
8. **Rebalance** has one cluster-wide lock (409 `rebalance_in_progress`). `maxConcurrentMoves` defaults to the
   cluster setting (4), range 1 to 64. The plan preview lists at most `moveLimit` moves and always reports totals.
   Removing a server and changing replica count do not rebalance implicitly except that `DELETE /servers/{id}` runs
   the drain rebalance as part of its own task. `GET /rebalance` reports `progress` only while a run is active and
   always `lastReport`, the nullable summary of the most recent finished, stopped or failed run.
9. **Auto-failover** settings live in `/settings/cluster` (`enabled`, `timeoutSeconds` 1 to 3600 default 120,
   `maxCount` default 1, read-only `eventsSinceReset`). The counter is reset by a rebalance.
10. **Network access** is replaced as a whole list with optimistic concurrency (`revision`); applying it on all
    servers is a task. TLS-required mode rejects plain `host` rules. Client certificates are expressed as
    method `cert`.
11. **Roles.** PostgreSQL roles are the only accounts. Membership of ordinary roles and of the four built-in cluster
    roles are separate replace-style sub-resources (`/memberships`, `/cluster-roles`). The last `kingres_admin`
    cannot be removed or disabled. Passwords are write-only and never returned.
12. **Query workbench** executes exactly one statement per request as the signed-in role; SQL errors are 422
    `sql_error` with `sqlState`. Large integers beyond 2^53 and bytea are returned as strings.
13. **Metrics** are one endpoint (`GET /metrics/query`) with a closed metric-name enum; max 5000 points per series.
    The backend answers from the bundled metrics store.
14. **Health.** `/healthz` and `/readyz` have no authentication and live under `/api/v1` like everything else. An
    uninitialized node is ready.
15. **Setup.** `GET /setup/state` is unauthenticated, tells the panel whether to show the wizard and reports the
    setup task (see "Setup flow"). `init-cluster` and `join-cluster` return tasks. Server addresses are `host:port`
    of the panel/REST port (default 8091 in the server template, configurable); the port is 1 to 65535.
16. **Idempotency keys** and ETags are not used; durable tasks provide idempotency (see above).
17. **Server identity** is the host of the catalog (see "Identifiers"). `Server.address` is the `host:port` of the
    REST API; its host part normally equals the id but the id, not the address, is the key.
18. **Create-database options.** `GET /database-create-options` lists valid encodings, collations (one list for
    `lcCollate` and `lcCtype`), the default bucket count with range, the default durability, the digest interval
    range and the cluster's replica count.

## Changelog

### 0.4.0

Revision written with the fourth increment of `kingresd` (design: `docs/design/control-plane-3.md`) and the fixes of
the security review of 2026-11-14. Summary for the panel: 0 operations added (86 operations, 69 paths), 1 breaking
change of a required role, 1 new required response property, 4 optional request properties.

#### Breaking

1. **`listLogs` and `listThisServerLogs` now need `kingres_ops`** (was `kingres_readonly`). The log of a PostgreSQL
   instance can contain data values of statements of other roles (finding CP-03 of the review); the panel must hide
   the Logs page from `kingres_readonly` and `kingres_stats` accounts. The text of statements itself is no
   longer written to the log of managed instances.
2. **`Task.detail` is a new required response property** (string or `null`): free text the running step recorded,
   for example `Waiting for server db3, which cannot be reached (Create the database on server db3).` or the
   certificate fingerprint seen when a server was added. Existing clients that ignore unknown properties are
   unaffected; a client that validates strictly must accept it.

#### Additive

1. **`JoinClusterRequest.expectedFingerprint` and `AddServerRequest.expectedFingerprint`** (optional string, SHA-256
   hex, colons allowed): pins the certificate of the other server on first contact. The header
   `X-Kingres-Pin-Sha256` of 0.3.0 still works; if both are given they must agree (400 otherwise). The seen
   fingerprint is in `Task.detail` of `server.add` and in the audit event (`fingerprintPinned`).
2. **Internal address convention** (stated in the descriptions of both requests): the internal API of a server is at
   its REST host and the port above its REST port. There is no `internalAddress` field.
3. **`TaskKind`** gains `replicate.apply` (see "Task kinds and subjects"; it was already used in 0.3.0 but hidden
   from the task list). Every kind the server uses is in the table, each with one subject type.
4. **`TaskStep.detail`** is now filled: for the running step it repeats `Task.detail`.
5. **`AlertKind`** gains `ledgerPendingTooOld`, `versionMismatch` and `certificateExpiring`
   (`ledgerVerificationFailed` existed).
6. **`LedgerVerification.taskId`** is nullable (a verification recorded by the extension without a task).
7. **`QueryResult.truncated`** is also `true` when the byte budget of the result (16 MiB) is reached, not only
   when `rowLimit` is; a single value larger than 1 MiB makes the statement fail with 422 `unprocessable`.

#### Documentation

1. `info.version` 0.4.0.
2. `TableClass`: **`plain` is never combined with another class** (a plain table with a ledger is `[ledger]`).
3. `removeServer`, `createInclusionProof`, server groups, certificate operations and the other items planned for
   0.4.0 are **not in this revision**; they answer `unsupported_operation` as before. See the server README, "Not done".
4. Role changes (`createRole`, `updateRole`, `deleteRole`, `setRoleMemberships`, `setRoleClusterRoles`) end the
   sessions and tokens of the changed role on every server at once; a role that is dropped, disabled or expired is
   refused on its next request (401); cluster roles are re-derived on every request (cache of 3 seconds).
   Membership in `pg_*` roles other than the harmless monitoring ones, and in roles with the SUPERUSER,
   REPLICATION or BYPASSRLS attribute, is refused with 400 `validation_failed`; the query workbench answers 403 for
   a role that has such an attribute or a membership giving access to server files, programs or logs.
   User names with control characters are refused by `login` with 400.

### 0.3.0

Revision written together with the second increment of `kingresd` (design: `docs/design/control-plane-2.md`).
Notation as in 0.2.0. Summary for the panel: 2 new operations (86 operations, 69 paths), 8 response properties
became nullable, 5 response properties are new and required, 1 request property is new and optional.

#### Breaking

1. **Response properties became nullable** (accept `null` = "not available"):
   - `Server.version` (the version of a remote server is unknown until it reports it);
   - `Database.createdAt`, `Database.createdBy` (databases created outside the control plane have no record);
   - `ClusterOverview.clusterId`, `ClusterOverview.name` (node state without cluster identity);
   - `LedgerStatus.mode`, `LedgerStatus.digestIntervalSeconds` (now consistent with `Database.ledger.*`: no catalog
     stores either, so the server answers `null` instead of an invented `optIn`/3600);
   - `LocalServerInfo.postgresVersion` (no instance is running).
2. **New required response properties** (the server must emit them; clients may rely on them):
   - `SetupTaskSummary.steps[]` (`name`, `state` in `pending|running|done|failed`), `createdAt`, `updatedAt`;
   - `TableColumn.blockingKeys[]` (index names of the primary and unique keys that lack the column; non-empty
     exactly when `distributionBlockedReason` is `notInUniqueKey`);
   - `InclusionProof.op` (`I` or `D`).
3. **`DistributionBlockedReason.notInUniqueKey` changed meaning** (clarified, previously ambiguous): the table has
   at least one primary or unique key that does not contain the column. A table without any unique key never
   has this reason, and a column that is part of every unique key can be distributed.

#### Additive

1. **`getClusterSummary`** (`GET /cluster/summary`, role `kingres_stats`): `ClusterSummary` (`clusterId`, `name`,
   `health`, `serverCount`, `databaseCount`, `openAlerts`), so that a statistics-only account has a cluster view.
2. **`enableTableLedger`** (`PUT /databases/{databaseName}/schemas/{schemaName}/tables/{tableName}/ledger`, role
   `kingres_admin`): body `EnableTableLedgerRequest` (`mode` `appendOnly|updatable`), answers 200 with the `Table`;
   one-way. New `AuditAction` value `enableTableLedger`.
3. **`InclusionProofRequest.op`** (`I` default, or `D`): which ledger entry of the row to prove; `D` proves a removal
   (updatable ledger tables).
4. Examples of `SetupTaskSummary` and `TableColumn` updated.

#### Documentation

1. `info.version` 0.3.0.
2. `Forbidden` response: the permitted addresses while `localhostOnly` are `127.0.0.0/8` and `::1`; every address
   otherwise.
3. `TableColumn.type` is the text of `format_type(atttypid, atttypmod)`, for display only.
4. `DatabaseCreateOptions.collations[].encodings`: `null` means every encoding, an empty array means none.
5. The meaning of `null` for the setup, ledger and table fields above is stated in each description.

### 0.2.0

Notation: `Schema.property` names a property in `components/schemas`; "server must emit" means that a response
schema now requires a property the 0.1.0 server did not send.

#### Breaking

1. **Server ids are strings.** New schema `ServerId` (`type: string`, 1 to 255 characters; the host of the server's
   instances in `kingres.node`). Changed from integer (int32) to `ServerId`:
   - path parameter `serverId` of `getServer`, `updateServer`, `removeServer`, `failoverServer`
     (`/servers/{serverId}`, `/servers/{serverId}/failover`); the component parameter `ServerId`;
   - query parameter `serverId` of `listBuckets`, `listLogs`, `queryMetrics`;
   - properties `Session.serverId`, `NodeStateInfo.serverId`, `LocalServerInfo.serverId`, `Server.id`,
     `Instance.serverId`, `ServerGroup.serverIds[]`, `ClusterOverview.managerLeader.serverId`,
     `Bucket.primaryServerId`, `Bucket.replicaServerIds[]`, `ServerBucketCount.serverId`,
     `ReplicationMember.serverId`, `ShardGroup.preferredPrimaryServerId`,
     `RebalancePlan.serverImpacts[].serverId`, `NetworkAccess.applyStatus[].serverId`,
     `ClusterSettings.restart.serverIds[]`, `LogEntry.serverId`, `Alert.serverId`,
     `MetricSeries.labels.serverId`;
   - `TaskSubject.id` for server subjects is the `ServerId`.
   The backend's "smallest `node_id` on the host" is replaced by the host string itself.
2. **Shard group ids are `group_id`.** The map group is group 0 (`ShardGroup.id: 0`, `Instance.shardGroupId: 0`),
   no longer 1; every other id is the catalog `group_id`, no more `group_id + 1`. `Instance.id` and
   `ReplicationMember.instanceId` stay `node_id`. Affects `Instance.shardGroupId`, `ShardGroup.id`,
   `Table.homeShardGroupId` (default home is the map group, 0), and every other `shardGroupId` field, which
   all use the same numbering.
3. **Request and response schemas are split** (item 7 of the review). New request schemas:
   `ShardingCreateSettings`, `ReplicationCreateSettings` (only `durability`), `LedgerCreateSettings`.
   `DatabaseCreateRequest.sharding|replication|ledger` now reference them. `ShardingSettings`,
   `ReplicationSettings`, `LedgerSettings` are response-only now; `ReplicationSettings.replicaCount` is a plain
   required response property (no longer `readOnly`) and appears in no request schema. A whole-file check found no
   other response-only field in a schema reused by a request (`ClusterSettings.autoFailover.eventsSinceReset`
   is `readOnly` but `ClusterSettings` is response-only; the update request is `ClusterSettingsUpdateRequest`).
4. **`GET /tasks/{taskId}` no longer accepts the setup token** (the `setupToken` security alternative is removed).
   Follow setup tasks with `GET /setup/state` (`setupTask`).
5. **Server must emit new required response properties**: `NodeStateInfo.setupTask` (null or `SetupTaskSummary`),
   `Session.clusterId` and `Session.clusterName` (nullable, but present), `RebalanceStatus.lastReport` (nullable,
   present), `Server.lastHeartbeatAt` (now required, nullable), and `columns` in the response of `getTable`.
6. **`getTable` returns `TableDetail`** (`Table` plus required `columns[]`) instead of `Table`. `listTables` still
   returns `Table` items without columns.
7. **Response properties became nullable** (clients must accept `null` = "not available"; servers return `null`
   instead of 0 or invented values):
   - `Server`: `uptimeSeconds`, `joinedAt`, `lastHeartbeatAt`, `sizeBytes`;
   - `ServerResourceSummary`: `cpuUsagePercent`, `memoryUsedBytes`, `memoryTotalBytes`, `diskUsedBytes`,
     `diskTotalBytes`; `CpuUsage`: `cores`, `usagePercent`, `loadAverage1m`; `MemoryUsage`: `totalBytes`,
     `usedBytes`, `quotaBytes`; `DiskUsage`: `totalBytes`, `usedBytes`;
   - `Instance.sizeBytes`;
   - bucket and table sizes: `DatabaseBucketCount.primarySizeBytes|replicaSizeBytes`,
     `ServerBucketCount.primarySizeBytes|replicaSizeBytes`, `ColocationGroup.sizeBytes`,
     `PlannedMove.estimatedSizeBytes`, `RebalancePlan.totals.totalBytes|estimatedDurationSeconds`,
     `BucketMove.sizeBytes`, `Table.sizeBytes`, `Table.estimatedRows` (`Bucket.sizeBytes` already was);
   - per-database settings: `DatabaseGeneral.owner|encoding|lcCollate|lcCtype|connectionLimit`,
     `ShardingSettings.bucketCount`, `ReplicationSettings.durability`, `LedgerSettings.mode|digestIntervalSeconds`;
   - per-database statistics: `DatabaseStats.sizeBytes|tableCount|activeConnections|transactionsPerSecond`,
     `DatabaseHealth.underReplicatedBuckets|unavailableBuckets`, `Database.ledgerSummary.verificationState`;
   - ledger: `LedgerChainStatus.verificationState`, `LedgerStatus.verificationState|ledgerTableCount`,
     `LedgerDigest.generatedBy`, `LedgerVerification.startedBy|blocksChecked|rowsChecked`.
   The request schemas keep non-null types.
8. **`AuditEvent.action` values** are `operationId`s (and `loginFailed`, `setupTokenRejected`, `autoFailover`), not
   dotted names; the type is the closed enum `AuditAction`, also for the `action` query parameter of `listAuditEvents`.
9. **`NodeAddress` validation tightened**: the port must be 1 to 65535 (the 0.1.0 pattern accepted 99999).
10. No `operationId` was renamed or removed. Schema names changed or added: see items 3 and 6 and "Additive".

#### Additive

1. **New operations** (84 operations, 67 paths now): `checkSetupToken` (`POST /setup/token-check`, scheme
   `setupToken`, rate limited, responses 204/401/403/422/429/503, no body) and `getDatabaseCreateOptions`
   (`GET /database-create-options`, role `kingres_readonly`, returns `DatabaseCreateOptions`: `encodings[]`,
   `collations[]`, `bucketCount {default 128, minimum 1, maximum 4096}`, `replicaCount`, `defaultDurability`,
   `ledgerDigestIntervalSeconds`). New schemas `DatabaseCreateOptions`, `IntegerRange`.
2. **Shared responses added to existing operations**:
   - 422 on 54 operations: `logout`, `getSession`, `getThisServer`, `getThisServerResources`,
     `listThisServerInstances`, `getThisServerInstance`, `restartThisServerInstance`, `stopThisServerInstance`,
     `startThisServerInstance`, `getThisServerSettings`, `leaveCluster`, `listThisServerLogs`, `getCluster`,
     `listServers`, `getServer`, `removeServer`, `failoverServer`, `listServerGroups`, `createServerGroup`,
     `deleteServerGroup`, `listDatabases`, `getDatabase`, `listTables`, `getTable`, `listBuckets`,
     `getBucketDistribution`, `getReplicationTopology`, `restorePreferredPrimaries`, `getRebalanceStatus`,
     `previewRebalancePlan`, `stopRebalance`, `listRebalanceReports`, `getRebalanceReport`, `listTasks`, `getTask`,
     `cancelTask`, `listClusterRoles`, `listRoles`, `getRole`, `deleteRole`, `setRoleClusterRoles`, `listGrants`,
     `applyGrantTemplate`, `getNetworkAccess`, `listAuditEvents`, `getClusterSettings`, `listLedgerStatus`,
     `listLedgerBlocks`, `listLedgerDigests`, `listLedgerVerifications`, `getLedgerVerification`, `listAlerts`,
     `acknowledgeAlert`, `listLogs` (meaning `unsupported_operation` among the other 422 codes; the backend's 29
     "not yet" operations are among them);
   - 429 on `login`, `initCluster`, `joinCluster`;
   - 503 on `logout`, `getSession`, `listClusterRoles`;
   - 403 on `logout`, `getSession` (CSRF, role) and on `initCluster`, `joinCluster` (non-permitted address while
     `localhostOnly`);
   - 400 on `restartThisServerInstance`, `stopThisServerInstance`, `leaveCluster` (optional body), `listServers`,
     `dropDatabase`, `getBucketDistribution` (query parameters);
   - 405 on every operation, via the new `components/responses/MethodNotAllowed` (`Allow` header, `Problem` body).
3. **`ProblemCode`** gains `method_not_allowed`.
4. **Setup**: `SetupTaskSummary` (`id`, `kind` `cluster.init|cluster.join`, `state`, `progressPercent`, `error`) and
   `NodeStateInfo.setupTask`; examples `initRunning`, `initDone`, `initFailed` of `getNodeState`.
5. **Session**: `Session.clusterId` and `Session.clusterName` in the login and `getSession` responses.
6. **Tables**: `TableDetail`, `TableColumn` (`name`, `type`, `nullable`, `canBeDistributionColumn`,
   `distributionBlockedReason`) and `DistributionBlockedReason` (`shardingDisabled`, `tableNotPlain`,
   `unsupportedType`, `notInUniqueKey`).
7. **Rebalance**: `RebalanceStatus.lastReport` (nullable `RebalanceReportSummary`); `progress` stays null while idle.
8. **Audit**: schema `AuditAction` (operation ids of all mutating operations except `previewRebalancePlan`,
   `createInclusionProof`, `checkSetupToken`, plus `login`, `logout`, `executeQuery`, and the events
   `loginFailed`, `setupTokenRejected`, `autoFailover`).
9. Script `scripts/check-contract.mjs` with `package.json` (dependency `yaml`).

#### Documentation

1. `info.version` 0.2.0; `info.description` states the uniform response rule, the identifier mapping and the
   meaning of `null`.
2. README: shared-response table with the exceptions; "Transport-level responses" (405, unknown path 404, 500);
   identifier table (`node_id`, `group_id`, string server id); "Task kinds and subjects" table; "Setup flow";
   "Audit actions"; durability to `synchronous_commit` table (`async`=`local`, `sync`=`on`, `syncApply`=`remote_apply`);
   default bucket count 128 (1 to 4096); design notes 2, 3, 5, 8, 15, 17 and 18 revised; how to run the check.
3. `GET /auth/session`: returns the session's current CSRF token and never rotates it; the security scheme text no
   longer says "login or `GET /auth/session`" issues it.
4. Descriptions of `login`, `getSession`, `getNodeState`, `initCluster`, `joinCluster`, `getTask`, `getRebalanceStatus`,
   `getTable`, `listAuditEvents` (`action` filter) and of the responses `UnprocessableEntity`, `TooManyRequests`,
   `Forbidden`, `AcceptedTask`; `TaskKind` and `TaskSubject.id` point to the mapping table; "the map group has id 1"
   removed everywhere (`Instance.shardGroupId`, examples now use 0).
5. Examples updated: server ids are host strings (`10.0.0.11`), the map group is group 0 in server, topology and
   slot examples (`kg_g0_i4`), audit `action: createDatabase`, rebalance status (`lastReport`, idle), table detail with
   columns, session with cluster identity.
6. Verified that the decisions "default bucket count 128, range 1 to 4096" and "`async`=`local`, `sync`=`on`,
   `syncApply`=`remote_apply`" hold everywhere in `openapi.yaml` (schema defaults, ranges, descriptions and
   examples); no leftover value was found in the YAML, the missing mapping in the README was added and the check
   script now guards both.
