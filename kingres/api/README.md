# KingresDB management REST API

`openapi.yaml` (OpenAPI 3.1, version 0.14.0) is the contract between the Go backend (`kingresd`) and the
Vue/TypeScript panel and, when it is built, the planned `kingres-cli` (it does not exist yet). Lint it and run the mechanical rule check with:

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
`synchronous_commit` `local`, `on`, `remote_apply`; and the 0.4.0 rules listed in the script (409 on the stop, start,
leave, remove, update-server, server-group and inclusion-proof operations, the new problem codes, the required and
nullable `Server` properties, the members of `InclusionProof` and of the digest document, the required
`NodeStateInfo.setupTask`, the closed update requests). Run it after every change to the contract and in CI.

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
- **Snake-case exception.** `InclusionProof` and `LedgerDigest.chains[].document` are the JSON documents of the
  `kingres_ledger` extension and keep its snake_case member names, so that they can be handed to `kingres-verify`
  unchanged. They are the only such exceptions.
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
| 409 | State conflict | `already_exists`, `immutable_field`, `rebalance_in_progress`, `operation_in_progress`, `task_not_cancellable`, `last_admin`, `role_in_use`, `optimistic_lock_failed`, `no_replica_available`, `instance_already_running`, `instance_already_stopped`, `server_owns_buckets`, `server_hosts_map_group`, `last_server`, `server_group_not_empty`, `ledger_entry_pending` |
| 422 | Well-formed but cannot be done | `unprocessable`, `sql_error`, `unsupported_operation`, `node_state_invalid` |
| 429 | Rate limit on unauthenticated endpoints | `rate_limited` |
| 503 | Cannot serve now | `node_uninitialized`, `map_group_unavailable`, `leader_unavailable`, `quorum_unavailable`, `service_unavailable`, `system_database_stopped` |

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
| `server.failover` (0.10.0 text) | `server` | the `ServerId` that is failed over; the report id is the task id. Steps: `Check that the failover can start`, then per group (`TaskStep.shardGroupId`) `Group <n>: Freeze the group`, `Stop the old primary`, `Wait for the old lease`, `Choose the standby`, `Hand the lease to the new server`, `Promote the standby`, `Make the catalog follow`, `Check the catalog`, `Point the other standbys at the new primary`, `Update the standby records`, `Check synchronous_standby_names`, `Resolve prepared transactions`, `Check that clients arrive`, `Lift the freeze`, then `Write the report`. A task that waits for an operator is `paused` and a repeat of the request resumes it; `Task.cancellable` is false once a standby has been promoted (the point of no return) |
| `certificate.renew`, `certificate.revoke` (0.5.0) | `server` | the `ServerId` whose certificates are renewed or revoked |
| `instance.start`, `instance.stop`, `instance.restart` | `instance` | the instance id (`node_id`) as a decimal string |
| `database.create`, `database.update`, `database.drop` | `database` | the database name |
| `table.distribute`, `table.undistribute`, `table.alter`, `table.partition` | `table` | `database/schema.table` (0.14.0: the table the request named; one table task per colocation group) |
| `rebalance` | `cluster` | `cluster` (0.9.0; one rebalance at a time, cluster-wide; the task id is the report id): steps `Check that the cluster can rebalance`, `Compute the plan`, `Move buckets`, `Write the report`; a stopped run is `paused` |
| `schema.distribute`, `schema.move`, `schema.undistribute` (0.16.0) | `schema` | `database/schema`: steps `Check the schema`, `Set REPLICA IDENTITY FULL`, `Make the schema distributed`, `Verify on every server`; `Check the move`, `Copy the data`, `Switch`, `Clean up the old copy`; `Check the schema`, `Move first`, `Make the schema local`, `Verify on every server`; one schema task at a time per schema |
| `bucket.split` (0.16.0) | `bucket` | `database/colocationId/bucket`: steps `Check that the split can start`, `Split the bucket`, `Check the new buckets`; one split per colocation group at a time |
| `replication.restorePreferredPrimaries` | `replication` | `preferred-primaries` (0.10.0: answers `unsupported_operation` until the switchover is built) |
| `ledger.verify` | `ledgerVerification` | the **database name**: the verification id exists only after the run (see `LedgerVerification.taskId`) |
| `ledger.key.create`, `ledger.key.rotate`, `ledger.key.retire`, `ledger.key.revoke`, `ledger.key.distribute` (0.11.0) | `ledgerKey` | `signing`: the signing keys of the ledger; one key task at a time, cluster-wide. `ledger.key.create` makes the first key (generate, store, distribute, event, activate); `ledger.key.rotate` adds the next key and starts the overlap; `ledger.key.retire` and `ledger.key.revoke` end a key; `ledger.key.distribute` brings the holders of the key up to date (started by `kingresd` when the holders change). No private key, seed or passphrase is ever in a step, a detail, a result or an error |
| `ledger.bundle.create`, `ledger.epoch.start`, `ledger.upgrade` (0.11.0; answer `unsupported_operation` until built) | `database` | the database name |
| `replicate.apply` | `replication` | `apply:<operation>:<role or database names>:<unix nanoseconds>`: the task that brings the servers that could not be reached up to date after a role or grant change (one step per server, `TaskStep.detail` names the server it waits for) |
| `networkAccess.apply` | `networkAccess` | `network-access` |
| `config.apply` (0.8.0) | `config` | `cluster`: one configuration task at a time; one step per server (`TaskStep.detail` names the server a step waits for) |
| `config.restart` (0.8.0) | `config` | `restart`: restarts the instances that wait for a restart, one server at a time |
| `standby.reconcile` (0.7.0) | `replication` | `standbys`: adds, removes and rebuilds standby instances until every group has `defaultReplicaCount` of them; the step `standby.register_in_catalog` ends in the step state `skipped` (0.9.0; `done` before) with the task `detail` `skipped: needs kingres_shard 0.5` until `kingresd` registers standbys in the sharding catalog |

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
for an unknown user), `setupTokenRejected` and `autoFailover` (actor `system`; 0.11.0 adds the operations `updateLedgerSettings`, `createLedgerSigningKey`, `retireLedgerSigningKey`, `revokeLedgerSigningKey`, `createLedgerAnchorTarget`, `updateLedgerAnchorTarget`, `deleteLedgerAnchorTarget`, `testLedgerAnchorTarget`, `anchorLedgerNow`, `retryLedgerAnchor`, `createLedgerBundle`, `registerLedgerClientKey`, `revokeLedgerClientKey`, `setTableSignaturePolicy`, `upgradeLedgerExtension` and `acknowledgeLedgerDivergence`, whose entries never hold a key, a passphrase or a credential; 0.10.0 adds the events `failoverGroupStarted`, `failoverGroupPromoted`, `failoverGroupFailed` and `oldPrimaryRebuilt`, resource type `shardGroup`). A denied or failed call of an
operation is recorded under that operation's id with `outcome` `denied` or `failure`. Read-only operations are not
audited except `executeQuery`. The list is the closed enum `AuditAction`, which is also the type of the `action`
filter of `GET /audit`.

## Inclusion proofs and `kingres-verify`

`POST /databases/{databaseName}/ledger/inclusion-proofs` answers with the proof document of
`kingres.ledger_inclusion_proof` (see `contrib/kingres_ledger/README.md`, "Inclusion proof") unchanged, with one added
member `shard_group_id` that the verifier ignores. The members are snake_case:

```
format, chain_id, relation (optional), table_id, ledger_txid, seq, op ("I"|"D"), row_hash,
entry_index, entry_count, entry_path[hex], tx_root, user_name, tx_index, tx_path[hex],
block {height, hash_version, prev_hash, tx_merkle_root, tx_count, closed_at_us, block_hash},
chain [ {height, hash_version, prev_hash, tx_merkle_root, tx_count, closed_at_us, block_hash?} ... ],
shard_group_id
```

The leaf of the row is `row_hash` (the entry leaf is derived from it, `seq`, `table_id` and `op`), the paths are lists of
sibling hashes without left/right marks (the side follows from `entry_index` and `tx_index` in trees of `entry_count`
and `block.tx_count` leaves), and the root of the proof is the `block_hash` of a digest of the same chain, reached
through `chain`. There is no digest reference in the proof: the digest is chosen by the verifier and must come from
outside the database (`generateLedgerDigest` returns it as `chains[].document`).

```
curl ... -X POST /api/v1/databases/shop/ledger/inclusion-proofs -d '{"schema":"app","table":"accounts","primaryKey":{"id":1}}' > proof.json
kingres-verify proof --proof proof.json --digest digest.json --table-id 1 --row-hash <hash recomputed from your own copy of the row>
```

The request names the row by `schema`, `table` and the whole primary key; the server reads the row as the signed-in role,
computes `ledger_row_hash` and asks for the proof (`ledgerTxId` selects a removed version of an updatable table,
`op: D` proves its removal). A row that is not in a block yet answers 409 `ledger_entry_pending`.

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
   `PATCH /servers/{id}` (`serverGroup`). Deleting a non-empty group is 409 `server_group_not_empty`; a duplicate
   name is 409 `already_exists`; a name that breaks the pattern is 400.
8. **Rebalance** has one cluster-wide lock (409 `rebalance_in_progress`). `maxConcurrentMoves` defaults to the
   cluster setting (4), range 1 to 64. The plan preview lists at most `moveLimit` moves and always reports totals.
   Removing a server and changing replica count do not rebalance implicitly: `DELETE /servers/{id}` removes a server
   that holds no buckets (409 `server_owns_buckets` otherwise); to empty one, set `shouldHaveShards: false` and run
   `POST /rebalance` first. `GET /rebalance` reports `progress` only while a run is active and
   always `lastReport`, the nullable summary of the most recent finished, stopped or failed run.
9. **Auto-failover** settings live in `/settings/cluster` (`enabled`, `timeoutSeconds` 1 to 3600 default 120,
   `maxCount` default 1, read-only `eventsSinceReset`). The counter is reset by a rebalance and by `resetAutoFailoverCounter` (0.10.0).
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

### 0.16.0

Revision written with the control plane of phases 10 (elasticity) and 11 (distributed schemas) of `kingres/docs/design/shard-phase10.md` and `shard-phase11.md`. It is built in increments; this
entry grows with them. Summary for the panel:
9 operations added (149 operations, 121 paths) (`drainServer`, `previewServerDrain`, `previewBucketSplit`, `splitBucket`, `listSchemas`, `getSchema`, `distributeSchema`, `undistributeSchema`, `moveSchema`); the six rebalance operations, `updateServer` and the cluster settings gain optional members; **no change that is not additive**.

#### Breaking

None.

#### Additive

- `RebalanceOptions` (the request of `startRebalance` and `previewRebalancePlan`, and the `options` of the status and the reports) has `strategy` (`byBucketCount` or `byDiskSize`, the new schema
  `RebalanceStrategy`), `threshold`, `drainOnly`, `maxMovesPerServer`, `acknowledgeFailures` and `cleanupDelaySeconds`; `RebalancePlanRequest` has `excludeServerIds` (plan as if these servers
  should not hold shards: the preview of a drain).
- `PlannedMove` has `cost` and `reason` (`balance` or `drain`); `RebalancePlan` has `strategy`, `threshold` and `imbalance`, and its server impacts `loadBefore` and `loadAfter`;
  `RebalanceStatus` has `strategy`, `threshold` and `trigger`; `RebalanceReportSummary` has `trigger`, `strategy` and `threshold`; a report move has `reason`.
- `Server` has the optional `weight`, and `updateServer` accepts it (above 0, at most 1000): the capacity of the server relative to the others. It needs the sharding extension 0.8.
- `drainServer` (`POST /servers/{serverId}/drain`, `kingres_ops`) and `previewServerDrain` (`GET /servers/{serverId}/drain-preview`): empty a server so that it can be removed. A drain is a task of kind
  `rebalance` (trigger `drain`), not a new kind: the status, the stop, the report and the exclusion of server operations are the rebalance's. New schemas `DrainRequest`, `DrainBlocker`, `DrainPreview`;
  new problem codes `replicas_cannot_be_kept`, `bucket_busy` and `split_unsupported` (the last two are for the split, below); `AuditAction` has `drainServer`.
- `previewBucketSplit` (`POST .../buckets/{bucket}/split-preview`, `kingres_readonly`) and `splitBucket` (`POST .../buckets/{bucket}/split`, `kingres_admin`, task kind `bucket.split`, subject type `bucket`, id
  `database/colocationId/bucket`): replace one bucket by 2, 4, 8 or 16 of a larger modulus on the same shard group. New schemas `BucketSplitRequest`, `BucketSplitPreview`, `BucketSplitChild`;
  `BucketState` has `splitting`; `ColocationGroup` has the optional `baseBucketCount`, `maxModulus`, `kind` and `schemaName` (`bucketCount` is now the current number of buckets); `Bucket` has `modulus` and
  `remainder`; `listColocationGroups` has the query `kind`; `TaskKind` has `bucket.split`, `TaskSubject.type` has `bucket`; `AuditAction` has `splitBucket`.
- Distributed schemas (phase 11), tag `Schemas`: `listSchemas` and `getSchema` (`kingres_readonly`), `distributeSchema` and `undistributeSchema` (`kingres_admin`), `moveSchema` (`kingres_ops`); new schemas
  `SchemaSummary`, `SchemaDistribution`, `SchemaDetail`, `SchemaBlocker`, `SchemaPage`, `DistributeSchemaRequest`, `MoveSchemaRequest`; task kinds `schema.distribute`, `schema.move`, `schema.undistribute` (subject type `schema`);
  problem codes `sharding_mode_in_use`, `schema_not_distributable`, `schema_pinned`, `schema_move_in_progress`. The sharding mode: `ShardingCreateSettings` and `ShardingSettings` have `mode` (`row`, `schema`, `both`; the new
  schema `ShardingMode`) and `distributeNewSchemas`, `bucketCount` is no longer required in a create request, `updateDatabase` takes `sharding.mode` and `sharding.distributeNewSchemas` (and `enabled` is optional),
  `DatabaseCreateOptions` has `shardingModes`, `DatabaseStats` has `distributedSchemaCount`, `TableDistribution.method` has `schema`.
- The automatic mode: `RebalanceSettings.automatic` (new schema `RebalanceAutomaticSettings`, with `RebalanceWindow`), updated through `RebalanceAutomaticSettingsUpdate`, and
  `RebalanceStatus.automatic` (`RebalanceAutomaticStatus`: whether it is on, when it looks next, what the last look decided and which rules held it back). `AlertKind` has `rebalanceFailed`.
- `RebalanceSettings` has `strategy`, `threshold`, `maxMovesPerServer`, `maxMoveAttempts`, `maxDiskUsedFraction` and `maxStandbyLagBytes`; `updateClusterSettings` takes them in the new
  `RebalanceSettingsUpdate`, in which every member is optional (`maxConcurrentMoves` was required before).

#### Documentation

- A client that does not know the new members works as before. The plan of an older extension (before 0.8) is still the plan by bucket count; asking for anything else is refused with 422 `unsupported_operation`.

### 0.14.0

Revision written with the control plane of the table lifecycle (increment 9.6 of `kingres/docs/design/shard-phase9.md`). Summary for the panel:
8 operations added (140 operations, 113 paths); one operation changed (`distributeTable`: a table with rows can be distributed, three optional request properties);
three task kinds, one problem code and one optional `Table` property added; **no change that is not additive**.

#### Breaking

None.

#### Additive

- `previewTableChange` (`POST .../tables/{tableName}/distribution/preview`): the blockers, warnings and notes of the report of the sharding extension and an estimate, without changing anything.
- `undistributeTable` (`DELETE .../distribution`, task kind `table.undistribute`) and `alterTableDistribution` (`PATCH .../distribution`, task kind `table.alter`),
  with the request properties `mode` (`auto`, `online`, `blocking`), `dependentViews` (`refuse`, `recreate`), `cascadeViaForeignKeys`, `targetGroupId`, `cascadeToColocated`.
- `listTablePartitions`, `createTablePartition`, `attachTablePartition`, `detachTablePartition`, `dropTablePartition` (task kind `table.partition`): the user partitions of a distributed table.
- `DistributeTableRequest` has optional `bucketCount`, `mode` and `dependentViews`; a table with rows can be distributed (online or blocking); a table that cannot is refused with
  422 `table_not_eligible` and `Problem.blockers` before the task exists.
- `ProblemCode` has `table_not_eligible`; `Problem` has optional `blockers`; `TaskKind` has `table.undistribute`, `table.alter`, `table.partition`; `Table` has the optional `partitioning`;
  `AuditAction` has the six new mutating operations.

#### Documentation

- The text of `distributeTable` says what changed: before 0.14.0 the table had to be empty. A client that does not know the new properties and codes works as before.
- `undistributeTable` and `alterTableDistribution` do not accept a user-partitioned table in this version (`table_not_eligible`).

### 0.13.0

Revision written with the failover of the map group (increment 5 of `kingres/docs/design/ha-phase5-part2.md`). Summary for the panel:
0 operations added (132 operations, 108 paths); one value added to an enum, the meaning of one id and the answer of one operation
extended; **no change that is not additive**.

#### Breaking

None.

#### Additive

- `AlertKind` has `certificateAuthorityUnavailable` (`critical`, subject `cluster`): the map group was failed over, the new manager leader
  holds no key of the certificate authority, and no certificate can be issued, renewed or revoked until the operator restores `pki/ca.key` and
  `pki/revoked.json` from the backup of the old leader. A client that does not know the kind shows it as an alert of an unknown kind.
- `failoverServer` for the server that runs the primary of the map group (the manager leader) is answered by any server that does not hear it:
  202 with a `Task` whose `id` is 0 while no task can be recorded (the steps of the bootstrap, `state` `running`), `Location: /api/v1/failover`.
  `GET /tasks/0` and `GET /failover` are answered by the server that runs the bootstrap until the task is recorded; `GET /failover` then names it
  in `taskId`. In mode `hard` only; `dryRun` and `graceful` answer 422 `unsupported_operation`.

#### Documentation

- The planned switchover (`failoverServer` with `mode: graceful`, ordinary groups): no change of schema. The text of the operation lists the steps and the
  task error codes `standby_behind`, `switchover_unproven`, `cannot_quiesce` and `prepared_transactions_pending`; `group_degraded` (409) is used for the
  first time (no standby streams from the primary); a request that includes the map group still answers 422 `unsupported_operation`. The console
  should show the pause of the writes (events `quiesced` and `writesResumed` with `pauseMs` in the report) and treat `Task.cancellable` as false from the promotion on.
- `Task.id`: 0 is the task that stands for the bootstrap; it never names a stored task.
- The console should show: the pseudo task (a progress bar of the eight steps B1 to B8 while `id` is 0, then the recorded task), the paused
  rebalance (`stoppedBy` `safetyRule`, text "The map group was failed over."), and the alert `certificateAuthorityUnavailable` with the
  steps of its message.

### 0.12.0

Revision written with the fixes for the review of the manual failover and the map group (kingresd). Summary for the panel:
0 operations added (132 operations, 108 paths); one value added to an enum, one optional property added to a schema, the text of one
state corrected; **no change that is not additive**.

#### Breaking

None.

#### Additive

- `FailoverReportSummary.outcome` has the value `completedWithWarnings`: every group was moved, and something that never fails a
  step was left undone or needs reading (a standby that could not be pointed at the new primary, a list of synchronous standbys that is
  still empty, prepared transactions that remain, an incident of `check_cluster`, a skipped call). A client that knows `completed`
  only must show it like `completed` and offer the report.
- `Problem.plan` (`FailoverPlan`, optional): present only on 409 `failover_needs_decision`; it is the plan that `dryRun` would answer, so
  that the console shows the groups, the candidates and the verdict of the refusal without a second request.

#### Documentation

- `FailoverOldPrimary.state` `notScheduled`: the text said that this revision does not rebuild the old primary. It does (the states
  `rebuildScheduled`, `rebuilding` and `standby` exist); `notScheduled` is now the record of an earlier revision and of a group whose
  old primary has no directory to rebuild.
- A failover and the creation or drop of a database, or the distribution of a table, exclude each other (409
  `failover_in_progress` for the database operations, `operation_in_progress` for the failover); the web console shows the message.

### 0.11.0

Revision written with phase 7 of the ledger, signing, publication and anchoring (steps 3 and 4 of the implementation plan of
`kingres/docs/design/ledger-phase7.md`; the design called the revision 0.9.0 before the numbers were taken). Summary for the panel:
28 operations added (132 operations, 108 paths); every operation answers 422 `unsupported_operation` until the version of
`kingresd` implements it (the signing operations, the settings and the trust file are the first); 5 problem codes, 9 alert kinds,
16 audit actions, 8 task kinds, 1 task subject type, **no change that is not additive** (the redefinition of `LedgerBlock.signed`
changes a value that was always false).

#### Breaking

None. `LedgerBlock.signed` keeps its name and type; it was always `false` because no server signature existed, and from 0.11.0 it
means that a signed checkpoint of the chain log covers the block.

#### Additive

- Operations: `getLedgerSettings`, `updateLedgerSettings`, `getLedgerSigning`, `createLedgerSigningKey`, `retireLedgerSigningKey`,
  `revokeLedgerSigningKey`, `getLedgerTrustFile`, `getLedgerPublication`, `listLedgerAnchorTargets`, `createLedgerAnchorTarget`,
  `getLedgerAnchorTarget`, `updateLedgerAnchorTarget`, `deleteLedgerAnchorTarget`, `testLedgerAnchorTarget`, `getLedgerAggregate`,
  `listLedgerAggregates`, `listLedgerAnchors`, `anchorLedgerNow`, `retryLedgerAnchor`, `downloadLedgerAnchor`, `createLedgerBundle`,
  `getLedgerBundle`, `listLedgerClientKeys`, `registerLedgerClientKey`, `revokeLedgerClientKey`, `setTableSignaturePolicy`,
  `upgradeLedgerExtension`, `acknowledgeLedgerDivergence`. `anchorLedgerNow` answers 201 with the records (the design said 202; a 202 of
  this contract is always a task).
- Schemas: `LedgerSettingsCluster`, `LedgerSettingsUpdate`, `LedgerSigning`, `LedgerSigningKey`, `LedgerSigningEvent`,
  `CreateLedgerSigningKeyRequest`, `RevokeLedgerSigningKeyRequest`, `LedgerTrustFile`, `LedgerPublication`, `LedgerAnchorTarget` (with its
  create, update and test shapes), `LedgerAnchor`, `LedgerAggregate`, `LedgerBundleRequest`, `LedgerClientKey` and the request shapes of the
  other operations.
- `LedgerStatus` gains the optional `publication` and `anchoring`; `LedgerVerifyRequest` gains `checks`; `LedgerDigest` gains
  `aggregateSize`; `TaskSubject.type` gains `ledgerKey`.
- Alert kinds `ledgerNotAnchored`, `ledgerAnchorBehind`, `ledgerAnchorFailed`, `ledgerAnchorConflict`, `ledgerChainDiverged`,
  `ledgerGroupUnread`, `ledgerPublicationBehind`, `ledgerSignerUnavailable`, `ledgerDigestOverdue`; problem codes `ledger_not_published`,
  `origin_prefix_locked`, `signing_key_missing`, `last_signing_key`, `chain_not_diverged`; task kinds `ledger.key.create`, `ledger.key.rotate`,
  `ledger.key.retire`, `ledger.key.revoke`, `ledger.key.distribute`, `ledger.bundle.create`, `ledger.epoch.start`, `ledger.upgrade`; the audit
  actions named under "Audit actions".

#### Documentation

Steps 5 and 6 of the plan (the collector, the chain logs, the aggregate and the publication) implement `getLedgerPublication`, `getLedgerAggregate` and
`listLedgerAggregates` **without a change of the contract**: the schemas were written with 0.11.0. `generateLedgerDigest` reads every shard group of the database
(a group that cannot be read is named in `uncoveredShardGroupIds` and the digest is `complete: false`; `503 service_unavailable` when no group answers),
`LedgerDigest.aggregateHash` is the hex of the root of the aggregate log and `aggregateSize` its size (both null while publication is off or no aggregate exists), and
`listLedgerDigests` lists the digests of every group (`id` is the identifier of that digest, not the one of a digest of a single chain). `LedgerAggregateMember.flags`
holds `stale`, `diverged` or `retired`; `LedgerAggregate.complete` is false and `uncoveredShardGroupIds` names the groups whose member is `stale` or `diverged`.
The three reads are answered by the manager leader (a follower forwards them). The files of a publication are not part of this contract: they are served by the
separate listener of `kingresd` in the layout of the C2SP tlog-tiles.

Implemented by `kingresd` with revision 0.11.0 first: `getLedgerSettings`, `updateLedgerSettings`, `getLedgerSigning`, `createLedgerSigningKey`,
`retireLedgerSigningKey`, `revokeLedgerSigningKey` and `getLedgerTrustFile` (the signing keys, step 4 of the plan); of the other 21 operations three are implemented by steps 5 and 6 (above) and the rest answer 422
`unsupported_operation` until their step is built. `getLedgerTrustFile` through a follower keeps `Content-Disposition`. The key tasks have the
subject `ledgerKey` with the id `signing`, and their task error codes are not an enum (clients tolerate codes they do not know): `key_set_damaged`,
`clock`, `term_over`, `not_leader`, `last_signing_key`, `key_files_remain`, `key_file_missing`, `signer_unavailable`, `cannot_sign`, `cannot_activate`,
`cannot_retire`, `cannot_revoke`, `trust_file`, `key_id_collision`.

Step 10 of the plan adds, **without a change of the contract**, `listLedgerAnchorTargets`, `createLedgerAnchorTarget`, `getLedgerAnchorTarget`, `updateLedgerAnchorTarget`,
`deleteLedgerAnchorTarget`, `testLedgerAnchorTarget`, `listLedgerAnchors`, `anchorLedgerNow`, `retryLedgerAnchor` and `downloadLedgerAnchor` (the WORM target; the
formats `ots` and `tsr` of the download answer 422 `unprocessable` until their targets are built). Decisions where the contract is silent: a target is created
disabled unless `enabled` is true, and enabling a target of a kind that needs credentials without them is 400 `validation_failed`; `credentials` is read once and never
returned (`credentialsSet` says whether they are stored; an empty object removes them, `null` or an absent member changes nothing); `config.credentialsPath` names the
operator's own credentials file instead; the state `claimed` of an OpenTimestamps record is shown as `submitted` in `LedgerAnchor.state` and never counts as anchored;
a record of a deleted target has `targetId: null` and keeps `targetName`; `retryLedgerAnchor` answers 409 `conflict` for a record that is not `failed` or `expired`;
a follower forwards all ten to the manager leader, which holds the term (503 with `Retry-After` while a new leader has none).

What is secret and what is not: a private key, a seed, a passphrase and a credential of an anchor target are never part of an answer, a task,
an audit entry or an error; the public key, the fingerprint and the trust file are public and the trust file is not a source of first trust
(an auditor compares the fingerprint with one obtained out of band). The signing keys are one set per cluster and live on the manager
leader and on its candidates; `getLedgerSigning.holders` names them. A key task is one at a time (409 `operation_in_progress`).

### 0.10.0

Revision written with high availability, part 2, the failover of an ordinary group driven by `kingresd` (increment 3 of
`kingres/docs/design/ha-phase5-part2.md`; the switchover, the map group, automatic failover and the old primary's return are
later increments and answer `unsupported_operation`). Summary for the panel: 4 operations added (104 operations, 86 paths):
`getFailoverStatus`, `listFailoverReports`, `getFailoverReport`, `resetAutoFailoverCounter`; `failoverServer` is implemented
for `hard` on ordinary groups; 2 problem codes, 7 alert kinds, 5 audit actions, **1 change that is not additive**.

#### Breaking

1. **`ClusterSettings.autoFailover.timeoutSeconds` has the minimum 30** (it was 1; the design always said 30, and a shorter value
   cannot tell a failure from a pause). Nothing was released.

#### Additive

- `FailoverRequest` gains `shardGroupIds`, `targets` (`FailoverTarget`), `allowDataLoss`, `dryRun` and `comment`; `failoverServer`
  and `restorePreferredPrimaries` answer 200 with a `FailoverPlan` for `dryRun`; `restorePreferredPrimaries` accepts an optional body.
- The schemas of the status and the report: `FailoverStatus`, `FailoverReport`, `FailoverReportSummary`, `FailoverGroupReport`,
  `DataLossAssessment`, `FailoverCandidate`, `FailoverIncident`, `OldPrimaryReport`, `FailoverEvent`.
- `ShardGroup.failover` (`FailoverReadiness`), `TaskStep.shardGroupId`, `ClusterSettings.autoFailover.lastEventAt`,
  `ClusterOverview.autoFailover.limitReached` and `lastEventAt`, `ClusterOverview.failover`.
- Alert kinds `failoverNeedsDecision`, `failoverBlocked`, `failoverDataLoss`, `splitPromotion`, `oldPrimaryRunning`,
  `oldPrimaryNotStopped`, `oldPrimaryNotRebuilt`; problem codes `failover_needs_decision` and
  `lease_settings_differ` (the existing `quorum_unavailable`, 503, is used too); audit actions `resetAutoFailoverCounter`, `failoverGroupStarted`, `failoverGroupPromoted`,
  `failoverGroupFailed`, `oldPrimaryRebuilt`.

#### Documentation

The meaning of `graceful` (the planned switchover), of `Task.cancellable` (false from the moment the task starts to stop the old primary of a group), of the counter's reset and
of the task steps of `server.failover` (one block per group, `TaskStep.shardGroupId`). The task error codes of the failover are
not an enum of the contract (clients tolerate codes they do not know): `superseded`, `lease_still_held`, `lease_held_elsewhere`,
`lease_lost`, `quorum_unavailable`, `no_candidate`, `target_not_available`, `split_promotion`, `promote_timeout`,
`promote_incomplete`, `bad_promote_target`, `standby_behind`, `prepared_transactions_pending`, `cancelled`. The failover
statuses in `FailoverReportSummary.outcome` add `paused` (a run that waits for an operator).

### 0.9.0

Revision written with phase 4 of the control plane, rebalancing driven by `kingresd`, and the defects that earlier revisions
and the console's adoption of 0.7.0 recorded. Summary for the panel: 0 operations added (100 operations, 82 paths), the six
rebalance operations that answered `unsupported_operation` are implemented, **4 changes that are not additive** (listed below,
acceptable because nothing has been released), 8 new problem codes, additive members listed below. Design:
`kingres/docs/design/shard-phase10.md` (first increment only: the strategy "by bucket count", one move at a time).

#### Breaking

Nothing was released, so these are corrections of the contract's own contradictions; a typed client must adapt:

1. **`GroupLease.holder` is nullable** (it stays required): it is `null` when `state` is `none`. It was a required string, which the
   server could not fill for a group that has no lease.
2. **`Server.lastHeartbeatAt`, `Bucket.bucket`, `ClusterSettings.defaultReplicaCount`, `ServerStatus` and the text of
   `updateServer` and `removeServer` change meaning in their descriptions** to what `kingresd` does. The ones a client can see:
   `draining` now means "should not hold shards and still runs the primary of a group with buckets" (it also was set for a server
   that only hosted standbys), and `decommissioning` is now set (while the removal task runs; it was never set).
3. **`TableDistribution.colocationId` and `bucketCount` are nullable** (they stay required): a reference table is in no colocation
   group. They were required integers, and a reference table could not be made before 0.9.0.
4. **`removeServer` and `updateServer` no longer count a stopped rebalance as running** (`rebalance_in_progress` only while a run is
   pending or running), and `server_owns_buckets` no longer counts standbys the server hosts. A client that waited for a stopped rebalance
   to end before it removed a server no longer has to.

#### Additive

1. **Rebalance** (`getRebalanceStatus`, `startRebalance`, `previewRebalancePlan`, `stopRebalance`, `listRebalanceReports`,
   `getRebalanceReport`): implemented. `RebalanceStatus.stoppedBy` and `pausedReason`, `RebalanceProgress.movesSkipped`,
   `RebalanceReportSummary.stoppedBy`, `error` and `totals.movesSkipped`, `RebalanceReport.placementBefore` and `placementAfter`
   (`PlacementEntry`), `PlannedMove`, `BucketMove` and the moves of a report gain `fromServerId` and `toServerId`, the moves of a report
   gain `attempts` and `skipReason`. Task kind `rebalance` has subject `cluster`/`cluster` (the table said `rebalance`/task id).
2. **`ProblemCode`** gains `nothing_to_rebalance`, `rebalance_not_running`, `cluster_inconsistent`, `version_mismatch`,
   `failover_in_progress`, `group_degraded`, `insufficient_disk` and `no_receiving_server`. `Problem.serverIds` (optional) names, for
   `leader_unavailable`, servers to open instead.
3. **`updateClusterSettings`** answers 200 as before, and 202 with the `Task` and `Location` when the update started
   `standby.reconcile` **and the request carries `Prefer: respond-async`**; a client that does not send it is not affected.
4. **Step state `skipped`** is now what `kingresd` records for a step that skipped itself (`standby.register_in_catalog`; it recorded
   `done` and put `skipped: ...` into the task detail). The text names `kingres_shard 0.5`, the version that provides the registration.
5. **`Database.ledgerSummary`** gains the optional `complete` and `uncoveredShardGroupIds` (the coverage the 0.7.0 changelog announced);
   **`AlertSubject.taskId`** (optional, nullable; `taskFailed` alerts about a failed rebalance); the format of `statementId` and the
   answer to a malformed one (400 `validation_failed`); what `queryMetrics` answers for a metric nobody samples (200, null points).
6. **`distributeTable`**: `method: reference` works (`kingres.create_reference_table`); the text says what the extension needs (an
   empty table for `hash`, no ledger table, the table restrictions) and the refusals are 422 `unprocessable` before the task starts.
7. **`Server.status`** `decommissioning` is set while `server.remove` runs, the dashboard's `ServerCounts.decommissioning` counts it,
   and `ClusterOverview.rebalance.running` and `taskId` are set.

   Review of the rebalancing work (additive, still 0.9.0): **`RebalanceReportSummary.outcome`** gains `cancelled` (a person cancelled the
   task of a stopped or running rebalance; `stoppedBy` is `operator`; no `taskFailed` alert), `Task.cancellable` is true for a `paused`
   task and `cancelTask` ends it at once, starting a stopped rebalance again begins a new round (attempts and skips do not carry over,
   the options of the request replace the old ones), `maxConcurrentMoves` defaults to 1, and a rebalance and a server add, remove, leave
   or failover are excluded atomically (409 `rebalance_in_progress`, `operation_in_progress` or `failover_in_progress` also when the other
   task is created at the same moment). Clients must tolerate an outcome they do not know.

#### Documentation

The README row of the task kind `rebalance`, the step state `skipped` and the extension version in the text of the standby task are
corrected. `Server.lastHeartbeatAt` says who heard the heartbeat (the answering server, not the manager). This version runs one
move at a time (`maxConcurrentMoves` is accepted and recorded); the strategies, weights, automatic mode, draining a server and splitting
a bucket of the design are later increments.

### 0.8.0

Revision written with phase 13, configuration from the console (PostgreSQL parameters at the cluster, server group, server and
database level, the clocks and time zones). Summary for the panel: 11 operations added (100 operations, 82 paths), **0 breaking
changes**, additive changes listed below, 8 new problem codes, 1 new alert kind, 2 new task kinds, 3 new audit actions. No property
was removed, renamed or made required. Design: `kingres/docs/design/config-phase13.md`.

#### Breaking

None.

#### Additive

1. **Operations** (tag `Configuration`): `listConfigParameters`, `getConfigParameter`, `listConfigChanges`, `listConfigServers`,
   `getConfigSummary`, `getClusterTime`, `listTimeZones` (`kingres_readonly`); `previewConfigChange`, `applyConfigChange`,
   `revertConfigChange`, `restartPendingInstances` (`kingres_admin`). The three that start a task answer 202 with the `Task` and a
   `Location`. `previewConfigChange` is a POST that changes nothing and is not audited.
2. **`TaskKind`** gains `config.apply` and `config.restart` (subject type `config`, see the table "Task kinds and subjects").
3. **`ProblemCode`** gains `config_parameter_unknown`, `config_parameter_managed`, `config_parameter_refused`,
   `config_level_not_allowed`, `config_value_below_floor`, `config_change_superseded`, `config_restart_order_conflict` and
   `nothing_pending`.
4. **`AlertKind`** gains `clockDifference`; **`AuditAction`** gains `applyConfigChange`, `revertConfigChange` and
   `restartPendingInstances`.
5. **`ClusterSettings.clock`** (optional) and `ClusterSettingsUpdateRequest.clock`: `maxDifferenceMs` (10 to 600000, default 1000).
   Changing it starts no task, so `updateClusterSettings` keeps answering 200 for it.
7. **`NodeStateInfo.commit`** (optional, nullable): the source commit of the build, 7 to 64 lower-case hexadecimal characters; the panel links the
   running version's source to it (AGPL-3.0-only, section 13).
6. A value that PostgreSQL hides from an ordinary database user (`hidden: true`) is never returned to any role, and values of
   `refused` parameters cannot be stored; nothing in the audit log can be a secret of this feature.

#### Documentation

The README has the two task rows. `kingresd` does not set the system clock of a server (that needs the operating system
administrator and is the job of NTP or chrony); the contract reports clocks and says so in `getClusterTime`.

### 0.7.0

Revision written with high availability, part 1 (heartbeats, group leases, standbys, the client entry point) and the three
gaps the panel reported against 0.6.0. Summary for the panel: 1 operations added (89 operations, 72 paths), **0 breaking
changes**, additive changes listed below, 1 new problem code (`workbench_busy`, which replaces a guess), 3 new alert kinds,
documentation corrections. No property was removed, renamed or made required.

#### Breaking

None. One status changes for a case that had no code of its own: the refusal of a workbench statement because the role or
the server has its slots full was 503 `service_unavailable` (a statement about the server's health, and what the panel
had to guess); it is now 429 `workbench_busy` (see Additive 3). A client that treated every 503 of `executeQuery` as
"retry later" should treat 429 `workbench_busy` the same way, with `Retry-After`.

#### Additive

1. **`Alert.subject`** (the gap in 0.6.0 that kept the panel from naming the server that is behind). A typed object
   `AlertSubject` with `serverId`, `behindServerId`, `shardGroupId`, `databaseName` and `aspect`, each null when
   it does not apply. For `replicationPending`, `serverId` is the leader that keeps the change waiting and
   `behindServerId` the server that is behind. `Alert.serverId` and `Alert.databaseName` stay and equal the subject's.
   The description of `AlertSubject` lists, for every kind, which members it sets.
2. **`LedgerStatus.complete` and `LedgerStatus.uncoveredShardGroupIds`.** The chains of a status are those of the shard
   group of the answering server; the other groups are listed, with the same meaning as on `LedgerDigest`. The
   database summary can state its coverage. (`LedgerChainStatus` is one chain and needs no such member.)
3. **`workbench_busy`** (429, with `Retry-After`), new `ProblemCode`, and `429` and `409` on `executeQuery`: a
   dedicated code for "this role already runs its two statements here, or the server runs four".
4. **`cancelStatement`** (`POST /query/statements/{statementId}/cancel`, role `kingres_readonly`) and
   `QueryRequest.statementId`: cancel one's own running statement. The server cancels the backend it tracks for that
   id; a client never names a process id. 204 on request, 404 when no such statement of the caller runs on this
   server. The cancelled `executeQuery` ends with 422 `sql_error` SQLSTATE `57014`. Not a task (the effect is immediate);
   audited as `cancelStatement`. New `AuditAction` values: `cancelStatement`, `leaseLost`, `leaseFenced`, `leaseAcquired`.
5. **Liveness.** `ServerStatus` text states `suspect` (silent for the suspect timeout, 5 s) and `dead` (the failure
   timeout, 10 s) and that the status returns when the server is heard again. `ServerCounts.suspect` is counted, the counts
   and the list come from one snapshot, `Server.lastHeartbeatAt` is set. The text of the status no longer says that every
   server answers the same: `suspect` and `dead` are the answering server's view. `leader_unavailable` (503) is now also
   the answer of a follower that has not heard from the leader for the leader-loss bound (30 s) to a request that needs a
   session check.
6. **Leases.** `ShardGroup.lease` (optional `GroupLease`: `holder`, `state` held/lost/granted/none, `remainingMillis`,
   `enforced`), the answering server's knowledge. New `AlertKind` values `leaseLost`, `groupWithoutSynchronousStandby`
   and `groupUnderReplicated`; `serverSuspect` and `serverDown` are now raised from heartbeats.
7. **Client entry point metrics**: `MetricName` values `clientConnectionsAccepted`, `clientConnectionsActive`,
   `clientConnectionsRefused`, `clientConnectFailures`, `clientConnectionsMoved`, `clientConnectionsIdleClosed`,
   `clientBytesIn`, `clientBytesOut` and the same with `clientStandby`.
8. **Standbys.** `TaskKind` `standby.reconcile`; `AuditAction` values `standbyCreated`, `standbyRemoved`, `standbyRebuilt`,
   `standbyInstanceStarted`, `standbyInstanceRemoved`, `standbyInstanceRebuilt`, `synchronousStandbyNamesChanged`,
   `replicationSlotDropped`; the text of `updateClusterSettings` (`defaultReplicaCount` starts the task, it does not only
   make a rebalance necessary) and of `ReplicationMember.instanceId` (1048576 plus the standby id while a standby is
   not in the sharding catalog). `replicationLagHigh` also covers a standby slot that retains too much WAL.

#### Documentation

- `autoFailover.timeoutSeconds` now has a real heartbeat source (automatic failover itself comes with the promotion
  in part 2; `failoverServer` and `restorePreferredPrimaries` still answer `unsupported_operation`).
- Deferred to a later revision: `LedgerChainStatus` coverage inside one chain, a cluster-wide view of leases.

### 0.6.0

Revision written with the fix round for the second independent review of `kingresd` and the defects the other engineers
reported. Summary for the panel: 0 operations added (88 operations, 71 paths), 2 breaking changes (both for security),
additive changes listed below, 1 new problem code, 1 new alert kind, documentation corrections. Everything else is additive: new optional
members of responses and requests, new members of enums that the contract already says clients must tolerate unknown
members of (`ProblemCode`, `AlertKind`). No property was removed, renamed or made required.

#### Breaking

1. **`addServer` needs `kingres_admin`** (0.5.0: `kingres_ops`). With `kingres_ops` an operator could make the cluster
   sign a certificate for a server the operator runs and hand it every role's password verifier, which is the way to
   `kingres_admin` (review CP2-02). **A client must:** show and call `addServer` only for administrators; an ops account
   now gets 403 `forbidden`. `joinCluster` was already administrator-only.
2. **`addServer` and `joinCluster` need a pin.** A request without `expectedFingerprint` (body) or the
   `X-Kingres-Pin-Sha256` header and without `allowUnpinned: true` is `400 validation_failed` with `errors[].field`
   `/expectedFingerprint` and code `required` (0.5.0 accepted it and trusted whoever answered first, which for a join
   sent the administrator's password to that peer: CP2-05). A malformed pin is `400` on every server (through a
   follower it used to become trust on first use, CP2-03). **A client must:** send the fingerprint (it can read the
   fingerprint of the server to add from that server's own panel, see additive 4 and 5) or offer an explicit "trust
   whatever answers" choice that sends `allowUnpinned: true`. Colons and upper case are accepted in the fingerprint,
   anything else that is not 64 hexadecimal digits is refused.

Behaviour that is not in the schema: a request that a follower forwards answers `401 unauthenticated` ("sign in again")
when the session can no longer prove itself to the leader (the role's password changed, or the role has no
SCRAM-SHA-256 password).

#### Additive

1. `AddServerRequest.allowUnpinned`, `JoinClusterRequest.allowUnpinned` (boolean, default false), see breaking 2.
2. `SetupTaskSummary.detail` (string or null, optional): the same text as `Task.detail`; for a join or an add it says
   whether a pin was checked and which fingerprint.
3. `ProblemCode` `internal_api_disabled` (409): a server that runs without `-internal-listen` is a configuration state,
   not a missing feature; `addServer`, `removeServer`, `leaveCluster`, `renewServerCertificates` and
   `revokeServerCertificates` answer it (0.5.0: 422 `unsupported_operation`, which the panel showed as "not
   implemented in this version").
4. `NodeStateInfo.internalFingerprint` (hex or null, optional): the fingerprint of the certificate of the internal API
   of an uninitialized server (its bootstrap certificate). It is answered only to a loopback caller while the panel is
   loopback-only, or to a request with a valid `X-Setup-Token`; null for everyone else.
5. `LocalServerInfo.internalFingerprint` (hex or null, optional): the certificate the internal API presents now,
   readable by any signed-in role.
6. `LedgerDigest.complete`, `LedgerVerification.complete` (boolean) and `uncoveredShardGroupIds` (int32 list), optional
   in the schema, always sent by 0.6.0: the shard groups whose ledger chain the digest or the verification did not
   cover. **A client must not show a `passed` verification with `complete: false` as proof for the whole database.**
   `aggregateHash` stays null for an incomplete digest.
7. `AuditEvent.serverId`, `AuditPage.partial`, `AuditPage.missingServers` (optional). `listAuditEvents` answers for the
   whole cluster from any server, like `listLogs`; event ids are unique in the cluster (an opaque number below 2^53,
   no sequence); an audit cursor of 0.5.0 is refused with 400 (cursors are not valid across revisions); an audited
   mutation answers 503 `service_unavailable` before anything is done when the audit log cannot be written.
8. `AlertKind` `replicationPending`: a change of roles (or other cluster state) has not reached a server that the leader
   still waits for. Subject `server:<leader>|behind:<server>`.
9. New schema `LedgerProofBlock` (allOf of `LedgerProofBlockHeader` and `block_hash`); `InclusionProof.block` refers to it.
   The wire format is unchanged; generators now produce a usable type instead of an intersection with an empty object.
10. `LocalSettings.dataPaths.walDirectory` and `logDirectory` are always present in the answer, `null` when they are the
    default (0.5.0 left them out); the `required` list is unchanged (`dataDirectory`).
11. **`getSession` and `listThisServerInstances` work while the system database is stopped** (they answered 503
    `system_database_stopped`; `startThisServerInstance` was the only exception). They answer for a session this
    process has already validated, with the roles of the last successful role check (at most one hour old), and
    never for a session or role this process has not checked, after any role change on the cluster, or past that
    age. `Session` gains optional `degraded` (boolean), `degradedReason` and `clientAddress`; `getSession` marks the
    answer `degraded: true` in that state. `listThisServerInstances` answers from the instance manager's memory
    (state from the manager; size and lag null; 503 if this process never read the list). The operations that work
    in the stopped state are exactly `startThisServerInstance`, `getSession` and `listThisServerInstances`; the state
    begins when the stop or restart is accepted. A client can drop the copy of the session JSON and CSRF token it
    kept in browser storage for this case, and the guess of the instance id before a start.
12. `Session.clientAddress` (optional): the TCP peer address of the request that made or read the session (behind a
    reverse proxy it is the proxy's address).
13. `replaceNetworkAccess` accepts the query parameter `dryRun=true` (any value other than true or false is 400): the
    same validation, 409 and 422 as a real change, answered 200 `NetworkAccessCheck {valid, revision}` without a task
    and without an audit event. A client can drop its own pre-check against locking itself out.

#### Documentation

1. Sessions and bearer tokens are per server: a token from server 1 gives 401 on server 2 (`login`, the two
   security schemes).
2. `LedgerDigest.chains[].document` must be kept byte-exact by clients: the verifier needs the exact bytes.
3. `Server.joinedAt` and `uptimeSeconds` are set for servers that joined (0.5.0 answered null). `ServerStatus.pendingRebalance`
   is "the server should hold shards, is the primary of no bucket and buckets exist"; it no longer depends on how far the
   copy of a neighbouring group's buckets has come, so `live` and `pendingRebalance` are deterministic.
4. `getDatabase` on a follower answers the same `sharding.bucketCount`, `ledger.mode` and `digestIntervalSeconds` as the
   leader (0.5.0 answered null on a follower; the contract text did not change).
5. `executeQuery`: a cut result of a statement that changes data is committed and reports its real command tag and
   `truncated: true` (0.5.0 rolled it back and answered a `SELECT n` tag); `timeoutMs` ends as 422 `sql_error` with
   SQLSTATE 57014 and never as 503; a role runs at most two statements at once; a value above 1 MiB or a row above 16 MiB
   is 422. `LedgerDigest.generatedBy` is the requesting role or `system` as the text always said (0.5.0 returned the
   service role).
6. While an instance is stopping or restarting, the only answer of the operations that need the system database is
   `system_database_stopped`.

### 0.5.0

Revision written with the fifth increment of `kingresd` (certificate renewal and revocation, cluster-wide logs, alerts
and metrics, the alert kinds that nothing raised). Summary for the panel: 2 operations added (88 operations, 71 paths), 0 breaking
changes, 16 additive changes, 2 new problem codes, 5 corrections of text. **Every change of the schema is additive**: new
operations, new optional response properties, new members of enums that the contract already says clients must
tolerate unknown members of (`TaskKind`, `AlertKind`, `AuditAction`, `ProblemCode`); no property was removed, renamed,
made required or given another meaning, and no operation changed its path, method or required role. A client generated
from 0.4.0 keeps working; it simply does not know the new members.

#### Breaking

None. (Three behaviours of the server changed to what 0.4.0 already said or implied; see "Documentation", 1 to 3.
A client that sent an alert cursor or a log cursor taken from a 0.4.0 server must start the list again: cursors are
opaque and are not valid across revisions, as the conventions always said.)

#### Additive

1. **`renewServerCertificates`** (`POST /servers/{serverId}/certificates/renewals`, `kingres_admin`, 202 + task
   `certificate.renew`): gives a server a new certificate now. **`revokeServerCertificates`**
   (`POST /servers/{serverId}/certificates/revocations`, `kingres_admin`, 202 + task `certificate.revoke`): revokes
   every certificate of the server and sends the revocation list to every server and instance. Both are described
   with what stops working and how a revoked server gets back in. The renewal job of the servers (a server renews
   its own certificate when less than a configurable part of its lifetime is left) has no operation: it is not
   controlled through the API.
2. **`TaskKind`** gains `certificate.renew` and `certificate.revoke` (subject type `server`, the server id; see the table
   "Task kinds and subjects").
3. **`ProblemCode`** gains `certificate_revoked` (409: `renewServerCertificates` and `revokeServerCertificates` for a
   server whose certificates are revoked) and `certificate_authority_unavailable` (503: the operation needs the
   certificate authority key and the manager leader does not hold it; also `addServer`, `joinCluster`, `removeServer`
   and `leaveCluster`, which need it too). The `Conflict` response lists the first.
4. **`MissingServer`** (new schema) and the optional properties **`partial`** (boolean) and **`missingServers`**
   (array of `MissingServer`) of `LogPage`, `AlertPage` and `MetricQueryResult`. A cluster-wide answer is built
   from the parts of every server; a server that did not answer within its deadline, could not be reached, refused
   or answered something invalid is named with its `reason` (`timeout`, `unreachable`, `refused`,
   `invalidAnswer`) and `partial` is true; the page holds what the others know. This is how a partial answer looks
   (0.4.0 had no way to say it, and the server answered for one server only).
5. **`listLogs`, `listAlerts` and `queryMetrics` answer for the whole cluster from any server**, as their summaries
   always said. `listLogs` now also applies `level` as "this level or more severe" and `q` (they were ignored or
   exact), orders by time, then server id, with a cursor that is a position in that order. `listAlerts` lists the
   alerts that every server keeps about itself and the cluster alerts of the manager leader. `queryMetrics`
   combines the samples of every server (a sum or an average by metric, as for one server).
6. **Alert ids** are unique in the cluster and name the server that holds the alert (the high bits are a number that
   the manager leader gave the server); ids stay below 2^53. `acknowledgeAlert` can be sent to any server.
7. **`AlertKind`** gains `certificateRenewalFailed` (the renewal job of a server failed three times in a row). The kinds
   `ledgerPendingTooOld`, `versionMismatch` and `certificateExpiring` were listed in 0.4.0 and are raised now, each with a
   condition, a clearing condition and one open alert per subject: see the README of `kingresd`, "Alerts raised since
   0.5.0".
8. **`AuditAction`** gains `renewServerCertificates`, `revokeServerCertificates` (operations) and the events that are not
   operations `certificateRenewed` and `certificateRenewalFailed` (actor `system`).
9. **`Server.certificateRevokedAt`** (optional, nullable date-time): when the certificates of the server were revoked.
10. `info.version` is 0.5.0.
11. The description of `Alert.id` says what the number means; the description of the three list operations and
    `queryMetrics` say what a cluster-wide answer is.
12. `listTables`, `listRoles` and `listGrants` page with a keyset in SQL (a hard cap on what one request reads); the
    wording of the cursor is unchanged (opaque), only a cursor of 0.4.0 is no longer valid.
13. The 503 problem `certificate_authority_unavailable` carries `Retry-After: 60`.
14. The `BadRequest` response states when an unknown member of a request body is `bad_request` and when it is
    `validation_failed` (see "Documentation", 3).
15. `renewServerCertificates` and `revokeServerCertificates` answer 404 for an unknown server, 409
    `operation_in_progress` while another task of the server runs, 409 `conflict` for a server that is not an active
    member; `revokeServerCertificates` refuses the manager leader's own server with 409 `server_hosts_map_group`.
16. The task table lists the two new kinds with their subject.

#### Documentation

1. **`leaveCluster` returns the server to `uninitialized`**, not `standalone` (0.4.0 said `standalone`; the server
   never did): its instance, key and certificates are deleted and it gets a new setup token.
2. **A forced leave without a reachable manager leader** answers 202 with a `Task` of `id` 0 (`state` `done`, one
   step) that cannot be fetched (`GET /tasks/0` is 404): no task can exist on a cluster that cannot be reached. The
   same holds when the leader refuses the revoked certificate of the server. Without `force` this is 503
   `service_unavailable`. The cluster keeps listing the server until `removeServer`.
3. **Unknown members of a request body.** For the requests the contract declares closed (`ServerUpdateRequest`,
   `LocalSettingsUpdate`, `InclusionProofRequest`) the answer is 400 `validation_failed` with `errors[].field` naming
   the member and `code` `unknown_property` (the server answered `bad_request` for `InclusionProofRequest`; fixed).
   For every other request body an unknown member is 400 `bad_request`, as before.
4. `listLogs` and `listThisServerLogs` order, filter and page as described above; a `partial` answer is still 200.
5. The revocation text states that a revoked server is still reachable by the leader for the revocation list and for
   its own removal, and that a server that owns buckets has no way back before a rebalance exists.

### 0.4.0

Revision written with the fourth increment of `kingresd` (design: `docs/design/control-plane-3.md`) and the fixes of
the security review of 2026-11-14. Summary for the panel: 0 operations added (86 operations, 69 paths), 7 breaking
changes (1 required role, 4 response schemas with new required properties or a new shape, 2 changes of behaviour that were
only described), 12 additive changes, 8 new problem codes. The revision number stays
0.4.0 (not released yet). Not in this revision: the operations to revoke and renew a certificate.

#### Breaking

1. **`listLogs` and `listThisServerLogs` now need `kingres_ops`** (was `kingres_readonly`). The log of a PostgreSQL
   instance can contain data values of statements of other roles (finding CP-03 of the review); the panel must hide
   the Logs page from `kingres_readonly` and `kingres_stats` accounts. The text of statements itself is no
   longer written to the log of managed instances.
2. **`Task.detail` is a new required response property** (string or `null`): free text the running step recorded,
   for example `Waiting for server db3, which cannot be reached (Create the database on server db3).` or the
   certificate fingerprint seen when a server was added. Existing clients that ignore unknown properties are
   unaffected; a client that validates strictly must accept it.
3. **`Server` has three new required response properties** (`GET /servers`, `GET /servers/{id}`,
   `PATCH /servers/{id}`): `certificateExpiresAt` (date-time or `null`: not available), `seenFingerprint` (64
   lower-case hex characters or `null`: no first contact recorded, for example the server that created the cluster)
   and `fingerprintPinned` (boolean; false whenever `seenFingerprint` is null). Clients that ignore unknown
   properties are unaffected.
4. **`InclusionProof` has a new shape** (nothing served it before: `createInclusionProof` answered 422
   `unsupported_operation`, so no client can depend on the 0.3.0 shape). The old camelCase members `leafHash`,
   `leafIndex`, `merklePath[] {hash, position}`, `transactionMerkleRoot`, `block` (`LedgerBlock`), `shardHeadPath`,
   `aggregateHash`, `digestId`, `hashVersion`, `shardGroupId`, `ledgerTxId`, `primaryKey`, `schema`, `table` are
   **removed**, with the schema `MerkleStep`: the extension's proof carries no leaf hash and no left/right positions
   (the side follows from the index) and no aggregate path. The body is now exactly the document of
   `kingres.ledger_inclusion_proof` with its snake_case members (`format`, `chain_id`, `relation`, `table_id`,
   `ledger_txid`, `seq`, `op`, `row_hash`, `entry_index`, `entry_count`, `entry_path`, `tx_root`, `user_name`, `tx_index`,
   `tx_path`, `block`, `chain`) plus one member `shard_group_id`; see "Inclusion proofs and `kingres-verify`" below.
   New schema `LedgerProofBlockHeader`. The rule "`InclusionProof.op` is required and is `I` or `D`" still holds.
5. **`LedgerDigest.chains[].document` is a new required response property**: the digest of that chain exactly as the
   extension returned it (`chain_id`, `height`, `block_hash`, ...), which `kingres-verify --digest` needs and the
   camelCase members do not hold.
6. **`removeServer` no longer drains.** The 0.3.0 text said that the task marks the server `draining`, rebalances and
   removes it. A server is removed only when it holds no bucket copy; otherwise 409 `server_owns_buckets`. To empty
   a server: `PATCH` `shouldHaveShards: false`, `POST /rebalance`, then `DELETE`. (This is what the server did; the old
   text was never implemented.) A `dead` server: 422 `node_state_invalid`.
7. **`leaveCluster` refuses a server that holds any bucket copy** (was: only the sole holder of a bucket), the same
   rule as `removeServer`; `force` no longer skips the three conflict checks, only the requirement that the rest of
   the cluster is reachable.

#### Additive

1. **`JoinClusterRequest.expectedFingerprint` and `AddServerRequest.expectedFingerprint`** (optional string, SHA-256
   hex, colons allowed): pins the certificate of the other server on first contact. The header
   `X-Kingres-Pin-Sha256` of 0.3.0 still works; if both are given they must agree (400 otherwise). The seen
   fingerprint is in `Task.detail` of `server.add`, in the audit event (`fingerprintPinned`) and now in
   `Server.seenFingerprint`.
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
8. **`ProblemCode`** gains `instance_already_running`, `instance_already_stopped`, `server_owns_buckets`,
   `server_hosts_map_group`, `last_server`, `server_group_not_empty`, `ledger_entry_pending` (all 409) and
   `system_database_stopped` (503). The enum description and the `Conflict` response list which operation answers
   which.
9. **409 `Conflict` is now listed by `updateServer` and `createInclusionProof`** (the other operations of the
   conflict list had it already).
10. **`LedgerInconsistency.kind`** gains `other` (a check of the extension with no kind of its own; `detail` starts
    with the check name).
11. **`ServerUpdateRequest`, `LocalSettingsUpdate` and `InclusionProofRequest` are closed**
    (`additionalProperties: false`): a member that is not listed is 400 `validation_failed`. Stated so that the
    server's refusal is specified; no client could send another member meaningfully.
12. **`InclusionProofRequest`**: `ledgerTxId` now means the transaction that removed a version (required for
    `op: D`), and `schema`, `table` have length limits. The member names are those of 0.3.0.

#### Documentation

1. `info.version` 0.4.0.
2. `TableClass`: **`plain` is never combined with another class** (a plain table with a ledger is `[ledger]`).
3. Role changes (`createRole`, `updateRole`, `deleteRole`, `setRoleMemberships`, `setRoleClusterRoles`) end the
   sessions and tokens of the changed role on every server at once; a role that is dropped, disabled or expired is
   refused on its next request (401); cluster roles are re-derived on every request (cache of 3 seconds).
   Membership in `pg_*` roles other than the harmless monitoring ones, and in roles with the SUPERUSER,
   REPLICATION or BYPASSRLS attribute, is refused with 400 `validation_failed`; the query workbench answers 403 for
   a role that has such an attribute or a membership giving access to server files, programs or logs.
   User names with control characters are refused by `login` with 400.
4. **Stopping the instance that hosts the system database** (decision: allowed, no flag). `stopThisServerInstance`
   accepts it; the API keeps answering from the process; every authenticated operation except
   `startThisServerInstance`, and `login`, answers 503 `system_database_stopped` until the instance is started;
   `startThisServerInstance` works from the in-memory session with the roles of the last successful check;
   `getHealth`, `getReadiness`, `getNodeState` keep answering; the stop task is recorded `done` just before the
   shutdown. Also stated: 409 `instance_already_stopped`, `instance_already_running`, `operation_in_progress` and
   `no_replica_available` (`force` overrides only the last one) on start, stop and restart.
5. **`leaveCluster` and `removeServer` conflicts** (409, checked in this order): `last_server`,
   `server_hosts_map_group` (an instance of shard group 0 on the server), `server_owns_buckets`, then
   `rebalance_in_progress` and `operation_in_progress`; `removeServer` answers 404 for an unknown server and 422
   `node_state_invalid` for a `dead` one.
6. **`updateServer`**: only `serverGroup` and `shouldHaveShards` can change; everything else is read-only (400);
   an empty body is 400; an equal value is a no-op 200; an unknown group is 422 `unprocessable` (`/serverGroup`);
   409 `operation_in_progress` while the server is being added, removed or left; 409 `rebalance_in_progress` while
   a rebalance runs.
7. **Server groups.** `createServerGroup`: duplicate name 409 `already_exists` (case-sensitive), pattern or length
   violation 400 `validation_failed`. `deleteServerGroup`: unknown group 404, group with servers 409
   `server_group_not_empty` (`detail` lists the ids); nothing changes on a refusal.
8. **Local settings.** `getThisServerSettings`: `restartRequired` is a persistent state until the instances are
   restarted. `updateThisServerSettings`: partial update of `memoryQuota`, `logLevel`, `logDirectory`; `logLevel`
   never needs a restart, the other two do (`restartRequired: true` in the response); an unknown or immutable
   member is 400; a quota the machine cannot hold or a log directory that is not an absolute existing writable
   directory is 422 `unprocessable`; the operation is local.
9. **Ledger verifications.** `listLedgerVerifications`: newest first, cursor paging as for every large list, bad
   cursor 400, `items: []` for a database without verifications; only finished runs have an id (a run in progress
   is followed through its task). `getLedgerVerification`: at most 100 inconsistencies, 404 for another
   database's run. `LedgerVerification`: the id is the control plane's; the extension's `clean`, `problems` and
   `failed` map to `passed`, `failed` and `failed`; `scope` is derived from the start height; `taskId` is the
   `ledger.verify` task that recorded the run and is never null for a run started with `startLedgerVerification`.
10. **`createInclusionProof`**: how the row is found (as the signed-in role, by full primary key; removed versions
    by the history row's `ledger_txid`, readable by the table owner only), the refusals (404, 403, 422, 409
    `ledger_entry_pending`) and the `kingres-verify` recipe; see "Inclusion proofs and `kingres-verify`".
11. `NodeStateInfo.setupTask` and `SetupTaskSummary` were specified in 0.2.0 and 0.3.0 and are unchanged; the server
    must emit them (see "Setup flow"). The check script now guards that `setupTask` is required.
12. The items that an earlier draft of this revision listed as "not in this revision" (`removeServer`,
    `createInclusionProof`, server groups) are specified now. Still not in this revision: the operations to revoke
    and renew a certificate; they do not exist in the contract.

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
