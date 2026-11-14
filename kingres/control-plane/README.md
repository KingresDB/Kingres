<!-- SPDX-License-Identifier: PostgreSQL -->
# kingresd: the KingresDB control plane

`kingresd` is the control-plane server of KingresDB. It serves the management
REST API that the web panel and `kingres-cli` use
(`kingres/api/openapi.yaml`, conventions in `kingres/api/README.md`) and the
embedded web panel, on one port (default `127.0.0.1:8091`). It runs on every
server of a cluster.

This is the **first increment**: the process skeleton, the complete routing of
the contract, authentication and authorization, the setup state machine, the
audit log, and the read side of the API backed by PostgreSQL. Everything that
changes the cluster answers with one uniform "not supported yet" problem (see
[What is implemented](#what-is-implemented)). The agent that manages PostgreSQL
instances, the task engine, rebalance, failover, the client router, etcd and the
CLI are not part of this increment.

> **Module path.** The Go module is `github.com/KingresDB/Kingres/kingres/control-plane`: the
> repository's location followed by the directory of this module.

## Layout

```
cmd/kingresd/            main: configuration, wiring, graceful shutdown
internal/
  config/                 flags + environment + JSON file, one table of settings
  contract/               operation table GENERATED from openapi.yaml (go generate)
    gen/                  the generator
  spec/                   OpenAPI loader used by the generator and by tests only
  problem/                RFC 9457 problem+json, the one error writer
  api/                    JSON request/response types of the contract
  httpapi/                router, guard pipeline, handlers, middleware
  auth/                   role hierarchy, sessions, CSRF, rate limiter, PostgreSQL login
  setup/                  node state machine and the one-time setup token
  audit/                  append-only audit log (JSON lines)
  testutil/               test helpers (temporary directories)
  store/                  Store interface (read side) and cursor helpers
    pgstore/              PostgreSQL implementation
    memstore/             in-memory fake for tests
  webui/                  go:embed of dist/ with SPA fallback
scripts/                  throwaway PostgreSQL instances for integration tests
.golangci.yml             linter configuration
DEPENDENCIES.md          third-party modules, versions, licenses
```

## Build, run, test

Go 1.27.3 (see `go.mod`); everything builds natively on Windows and Linux.

```
go build ./...
go build -o kingresd ./cmd/kingresd
./kingresd -pg-dsn "host=127.0.0.1 port=5432 dbname=postgres user=kingresd password=..." -data-dir ./data
```

On first start while the node is `uninitialized`, `kingresd` writes the
one-time setup token to `<data-dir>/setup-token` (owner-only) and logs only the
file name. `GET /api/v1/setup/state` and `/healthz` work immediately.

Tests:

```
go vet ./...
gofmt -l .                      # must print nothing
golangci-lint run ./...
go test ./...                   # no database needed; integration tests skip
CGO_ENABLED=1 go test -race ./...   # needs a C compiler (gcc) on PATH
```

Integration tests against a real PostgreSQL run when `KINGRES_TEST_DSN` is set.
`scripts/run-integration.ps1` does everything: it starts a primary and a
streaming standby from a KingresDB build in WSL2 on ports 56010/56011
(`scripts/pg-test-instance.sh`, SCRAM, fixture in `scripts/fixture_*.sql`),
runs `go test` with the DSN, and always removes the instances.

```
.\scripts\run-integration.ps1 [-Port 56010] [-Race]
```

To change the contract-derived operation table after `openapi.yaml` changed:
`go generate ./internal/contract`. A test fails while the checked-in table is
stale.

## Configuration

Precedence, lowest to highest: built-in default, JSON config file (`-config` or
`KINGRES_CONFIG`), environment variable, command-line flag. The keys of the
config file are the flag names. Unknown keys and invalid values stop the
server at start. The DSN is never logged.

| Flag / file key | Environment variable | Default | Meaning |
|---|---|---|---|
| `-config` | `KINGRES_CONFIG` | none | JSON config file |
| `listen` | `KINGRES_LISTEN` | `127.0.0.1:8091` | host:port of the REST API and the panel |
| `pg-dsn` | `KINGRES_PG_DSN` | `host=127.0.0.1 port=5432 dbname=postgres` | DSN of the local map-group instance; the database of the DSN holds the cluster catalog. May contain a password |
| `data-dir` | `KINGRES_DATA_DIR` | `kingresd-data` | node state, setup token, audit log |
| `audit-file` | `KINGRES_AUDIT_FILE` | `<data-dir>/audit.jsonl` | audit log |
| `log-level` | `KINGRES_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `log-format` | `KINGRES_LOG_FORMAT` | `text` | `text` or `json` |
| `session-ttl` | `KINGRES_SESSION_TTL` | `8h` | absolute lifetime of sessions and tokens |
| `max-sessions` | `KINGRES_MAX_SESSIONS` | `10000` | bound on live sessions |
| `read-header-timeout` | `KINGRES_READ_HEADER_TIMEOUT` | `10s` | |
| `read-timeout` | `KINGRES_READ_TIMEOUT` | `30s` | |
| `write-timeout` | `KINGRES_WRITE_TIMEOUT` | `60s` | |
| `idle-timeout` | `KINGRES_IDLE_TIMEOUT` | `120s` | |
| `request-timeout` | `KINGRES_REQUEST_TIMEOUT` | `30s` | processing time of one API request |
| `shutdown-timeout` | `KINGRES_SHUTDOWN_TIMEOUT` | `15s` | graceful shutdown |
| `max-body-bytes` | `KINGRES_MAX_BODY_BYTES` | `1048576` | request body limit |
| `login-rate-limit` | `KINGRES_LOGIN_RATE_LIMIT` | `10` | login attempts per client address and per user name per window |
| `login-rate-window` | `KINGRES_LOGIN_RATE_WINDOW` | `1m` | |
| `tls-cert-file`, `tls-key-file` | `KINGRES_TLS_CERT_FILE`, `KINGRES_TLS_KEY_FILE` | none | enable HTTPS (both or neither) |

The role in the DSN is the **service role**: it reads the catalogs. It needs
`CONNECT` on the databases, `SELECT` on the `kingres` schema, and
membership in `pg_monitor` (for `pg_stat_replication`, `pg_stat_file`, sizes).
It is not used to authenticate users.

## Request pipeline

Every operation of the contract is routed (`/api/v1/...`) through one guard
(`internal/httpapi/guard.go`), in this order:

1. node-state gating: while `uninitialized` only `/healthz`, `/readyz` and
   `/setup/*` work; everything else is 503 `node_uninitialized`, before any
   authentication (`GET /tasks/{id}` no longer accepts the setup token: the
   wizard follows setup through `GET /setup/state`);
2. cross-site check (`Sec-Fetch-Site: cross-site` on a state-changing request is
   refused);
3. rate limit for operations without a session (per client address);
4. authentication: bearer token, session cookie, or the setup token;
5. CSRF check for cookie sessions on POST/PUT/PATCH/DELETE (`X-CSRF-Token`);
6. role check against `x-required-role` of the operation, with the hierarchy
   admin > ops > readonly > stats;
7. query-parameter validation (unknown parameters are 400);
8. the handler; calls of the audited operations (the `AuditAction` enum) by an
   authenticated caller are audited.

The minimum role of every operation comes from the generated table
(`internal/contract/operations_gen.go`), which is generated from
`openapi.yaml`: there is no hand-maintained copy.

Wrong method on a known path is 405 `method_not_allowed` with an `Allow`
header, answered before authentication; unknown routes are the contract's 404 problem;
panics become 500 `internal_error` and are logged with the stack; every
response carries `X-Request-Id`, copied into `Problem.requestId` and into audit
events; access logs are structured and contain no query strings, headers or
bodies.

## What is implemented

The server implements revision **0.2.0** of the contract (84 operations, 67
paths). "Real" means the operation works against PostgreSQL or the local node
state. "Not yet" means: after authentication, CSRF and role checks have passed
the server answers **HTTP 422 with `code: unsupported_operation`**
(`Operation <id> is not supported by this server version yet.`), which every
authenticated operation lists in 0.2.0. **33 of 84 operations are real; 51
answer "not yet".**

| Tag | Real | Not yet |
|---|---|---|
| Health | `getHealth`, `getReadiness` | |
| Auth | `login`, `logout`, `getSession` | |
| Setup | `getNodeState` (with `setupTask`), `checkSetupToken`; setup-token check, state gating and audit of `initCluster`, `joinCluster` | the cluster initialization and join themselves (`initCluster`, `joinCluster`) |
| This server | `listThisServerInstances`, `getThisServerInstance` (from `kingres.node`) | `getThisServer`, `getThisServerResources`, instance `start`/`stop`/`restart`, local settings, `leaveCluster`, local logs |
| Cluster | `getCluster` | |
| Servers | `listServers`, `getServer` | `addServer`, `updateServer`, `removeServer`, `failoverServer` |
| Server groups | `listServerGroups` | `createServerGroup`, `deleteServerGroup` |
| Databases | `listDatabases`, `getDatabase`, `getDatabaseCreateOptions` | `createDatabase`, `updateDatabase`, `dropDatabase` |
| Tables | `listTables`, `getTable` (with `columns`) | `distributeTable` |
| Shards | `listColocationGroups`, `listBuckets`, `getBucketDistribution` | |
| Replication | `getReplicationTopology` | `restorePreferredPrimaries` |
| Rebalance | | all six operations |
| Tasks | `listTasks`, `getTask` (from `kingres.task`, empty if absent) | `cancelTask` |
| Roles | `listRoles`, `getRole`, `listClusterRoles` | `createRole`, `updateRole`, `deleteRole`, `setRoleMemberships`, `setRoleClusterRoles` |
| Grants | `listGrants` | `createGrant`, `revokeGrants`, `applyGrantTemplate` |
| Network access | | both operations |
| Audit | `listAuditEvents` | |
| Settings | `getClusterSettings` | `updateClusterSettings` |
| Ledger | `listLedgerStatus`, `getLedgerStatus`, `listLedgerBlocks` | digests (list, generate), verifications (list, start, get), `createInclusionProof` |
| Query | | `executeQuery` |
| Alerts | | both operations |
| Logs | | `listLogs` |
| Metrics | | `queryMetrics` |

### How the catalog maps to the contract

The identifiers follow the contract without arithmetic: an instance id is
`node_id`; a shard group id is `group_id` (the map group is 0); a server is one
distinct `host` of `kingres.node` and its id is that host string, unchanged.
The server address is `host:<port of listen>`. Server status: all nodes
inactive is `dead`; not `should_have_shards` while holding buckets is
`draining`; a live server holding no buckets while a bucket map exists is
`pendingRebalance`; else `live`. `suspect` and `decommissioning` need the
manager and are never reported.

### Values the server does not have are `null`

0.2.0 makes "not available" a first-class answer, and the server uses it
instead of inventing zeros. These are always `null` in this version, because no
catalog or agent provides them. The list is enforced by
`TestIntegrationValuesWithoutSourceAreNull`, which fails when one of them
becomes non-null:

- `Server`: `joinedAt`, `lastHeartbeatAt`, `sizeBytes`,
  `replicationLagMaxSeconds` and all of `resources`; `uptimeSeconds` for every
  server except the one this process runs on; `Instance.sizeBytes`;
  `ClusterOverview.managerLeader` (there is no manager yet).
- Bucket and bucket-distribution sizes (`Bucket.sizeBytes`,
  `ServerBucketCount.*SizeBytes`, `DatabaseBucketCount.*SizeBytes`) and
  `Bucket.movingToShardGroupId`.
- `Table.sizeBytes`, `Table.estimatedRows` and `ColocationGroup.sizeBytes` of
  distributed and reference tables when the cluster has more than one shard
  group (this node sees only its own partitions); `estimatedRows` of a table
  that was never analyzed (`reltuples = -1`).
- `Database.ledger.digestIntervalSeconds`; `Database.ledger.mode` once a ledger
  catalog exists (it is `off` when none does); `stats.transactionsPerSecond`;
  `ledgerSummary.*`; `sharding.bucketCount` of a database without colocation
  groups.
- Ledger verification state and time (`LedgerStatus`, `LedgerChainStatus`).
- `NodeStateInfo.setupTask` until a setup request was accepted. Initialization
  is not implemented, so it stays null; its persistence and response are in
  place and tested.

Everything else is read from PostgreSQL:

- **Database settings**: owner, encoding, locale, connection limit and size come
  from `pg_database`; `sharding.enabled` is "the shard catalog exists in that
  database"; durability comes from a per-database `synchronous_commit` setting
  or the server default; `createdAt` is the modification time of the database
  directory's `PG_VERSION` file (the postmaster start time without
  `pg_monitor`, see "Contract notes").
- **Cluster settings**: the replica count is the largest number of standbys of
  any group (at most 3); auto-failover and rebalance values are the contract
  defaults; features are the existence of the shard and ledger catalogs.
- **Ledger**: only the chain of the instance behind the DSN is read; the other
  chains of a sharded database need the agent. A block `prev_hash` of 32 zero
  bytes is reported as `null`; `signed` is `false`.
- **Replication topology**: read live from `pg_stat_replication` and
  `pg_replication_slots` on each group's primary (connection with the service
  credentials to the host and port in `kingres.node`). A standby is matched to
  its walsender by `application_name` = `kingres_node_<node_id>` (also
  `node_<id>`, `node<id>`, the bare id) or by client address = host. The
  preferred primary server of a group is the host of its lowest node id. An
  unreachable primary makes the group `degraded`, not an error.
- **`getTable` columns**: from `pg_attribute`. A column can be the distribution
  column when sharding is enabled, the table is plain, the type has a default
  hash operator class with an extended hash function (domains resolve to their
  base type; `varchar` is hashed as `text`, `cidr` as `inet`; arrays, enums and
  ranges through their polymorphic classes) and the column is a key column of
  every primary or unique index of the table (PostgreSQL requires the
  partition key in every unique index). Otherwise the first applicable reason
  of `shardingDisabled`, `tableNotPlain`, `unsupportedType`, `notInUniqueKey`
  is returned. A type that PostgreSQL hashes through some other binary
  coercion is reported as `unsupportedType`.
- **`getDatabaseCreateOptions`**: encodings from `pg_encoding_to_char` without
  the client-only encodings (BIG5, GBK, GB18030, JOHAB, SJIS, SHIFT_JIS_2004,
  UHC); locales from the libc rows of `pg_collation` (a name that fits every
  encoding has `encodings: null`); the defaults are the encoding and locale of
  `template1`; bucket count 128 (1 to 4096), digest interval 3600 (60 to
  86400), and the replica count and default durability of the cluster
  settings.
- **`kingres.task`** is not specified by the design notes. The read side
  assumes `task_id bigint, kind text, state text, progress jsonb, error jsonb,
  subject jsonb, cancellable bool, created_at, updated_at timestamptz,
  created_by text`, with the jsonb columns holding the `TaskProgress`,
  `TaskError` and `TaskSubject` objects of the contract (see
  `scripts/fixture_catalog.sql`). If the table does not exist the task list is
  empty and every task is 404.
- **Missing catalog objects** (`kingres.node`, `bucket_map`, `ledger_*`, ...)
  give empty but valid responses.

## Contract types and validation

**Choice: hand-written types in `internal/api` plus contract tests that
validate real responses against the schemas of `openapi.yaml`.**

The code generator that was evaluated is `oapi-codegen` v2.8.2 (the usual
choice, based on kin-openapi). Run against this file it stops with
`unhandled Schema type: &[string number boolean]` (`InclusionProofRequest.primaryKey`),
that is, it does not support the OpenAPI 3.1 multi-type form `type: [string, number, boolean]`
(a property that accepts several JSON types), which the contract uses. Rewriting
the contract to suit a generator was not an option. Other generators were not
tried against this file; none known to the author claims complete 3.1 support.

What keeps the hand-written types honest:

- `internal/httpapi/contract_test.go` loads `openapi.yaml`, calls every
  implemented operation (with seeded fake data and, in the integration tests,
  with real PostgreSQL data) and validates each response body, and the
  `application/problem+json` bodies of the error paths, against the schemas of the
  contract with a JSON Schema 2020-12 validator (`santhosh-tekuri/jsonschema`,
  with format assertions). A type that drifts (a `null` where an array is
  required, a missing required property, a wrong enum) fails the test; a second
  test proves the validator can fail.
- The operation table (paths, methods, roles, security) is generated from the
  contract, not typed.
- A routing test parses `openapi.yaml` and fails if an operation is not routed
  or if the server routes something that is not in the contract; the server also
  refuses to start with a handler for an unknown operation.

If a 3.1-capable generator appears, `internal/api` is the only place to replace.

## Security notes

Things a reviewer should look at first:

- **Login = a PostgreSQL connection as the user** (`internal/auth/postgres.go`).
  SCRAM is done by the driver; the password is used for that one connection and
  dropped, never logged, stored, or audited. The cluster roles are then read by
  walking `pg_auth_members` explicitly (direct and inherited). `pg_has_role`
  is deliberately not used: it answers true for every role when the user is a
  superuser, which would make every superuser a `kingres_admin`. A role with
  no cluster role is refused (403). Wrong user, wrong password and `NOLOGIN`
  are indistinguishable (401 `invalid_credentials`). **The `pg_hba.conf` of the
  instance must not use `trust` for the addresses `kingresd` connects from**,
  or every password is accepted.
- **Sessions** are server-side, in memory (a restart signs everyone out). The
  credential is 256 random bits; the store keeps only its SHA-256. Absolute
  expiry (no sliding). The cookie `kingres_session` is `HttpOnly`, `Secure`,
  `SameSite=Strict`, `Path=/api/v1`. `Secure` means browsers send it only over
  HTTPS or to `localhost`; use `tls-cert-file`/`tls-key-file` or a TLS proxy.
  A bearer token and a cookie are separate sessions and cannot be swapped: a
  cookie value presented as a bearer token is refused, so the CSRF-exempt path
  cannot be used with a stolen cookie.
- **CSRF**: cookie sessions need `X-CSRF-Token` (constant-time comparison) on
  every POST/PUT/PATCH/DELETE; bearer requests do not. In addition any
  state-changing request with `Sec-Fetch-Site: cross-site` is refused.
  `GET /auth/session` returns the current token (the cookie is HttpOnly, so a
  reloaded panel fetches it there); it is not rotated.
- **Rate limits**: login per client address and per user name (a successful
  login resets only the user's counter, not the address' history); the
  unauthenticated operations per address; failed setup-token attempts per
  address (valid polling is not counted). Limiter memory is bounded. The client
  address is the TCP peer: `X-Forwarded-For` is **not** trusted (there is no
  trusted-proxy setting yet), so behind a reverse proxy all clients share the
  proxy's address.
- **Setup token**: 192 random bits, generated on first start while
  `uninitialized`, written with an exclusive atomic create, mode 0600 (on
  Windows the ACL of the data directory applies; the directory itself is
  created 0700), compared as SHA-256 digests in constant time, never logged,
  removed when the node leaves `uninitialized`.
- **Gating before authentication**: an uninitialized node answers 503 to
  everything but health and setup.
- **Setup operations** (`checkSetupToken`, `initCluster`, `joinCluster`): a
  missing or wrong token is 401 `setup_token_invalid`; once the node left
  `uninitialized` they are 422 `node_state_invalid`; while the panel is
  localhost-only a non-loopback caller is 403; failed attempts per address are
  rate limited (429), and the check never consumes the token.
- **Audit**: JSON lines, append-only (opened with `O_APPEND`, fsynced per
  event, a torn final line from a crash is terminated and skipped). Recorded:
  every login attempt that reached PostgreSQL (success, failure, denied), every
  mutating request of an authenticated caller (including denied ones), every
  request of the setup operations that presented a token (a wrong token is
  `setupTokenRejected`). Not recorded, only logged: malformed or rate-limited
  logins, missing or rate-limited setup tokens and unauthenticated 401 requests,
  so an anonymous client cannot grow the file. Only the operations of the
  contract's `AuditAction` enum are audited (`previewRebalancePlan`,
  `createInclusionProof`, `checkSetupToken` are not), and `loginFailed` is the
  action of every rejected sign-in. An audit write failure is logged; the response is not changed because the
  action has already happened. The audit `action` is the operation id
  (`createRole`) or `loginFailed`/`setupTokenRejected`; the `action` filter of
  `GET /audit` accepts exactly the contract enum, which is generated into
  `internal/contract`.
  Reading the audit log scans the whole file (fine for this increment).
- **Input handling**: bodies are size-limited, JSON is decoded strictly (unknown
  fields, trailing data and wrong content type are 400), unknown query
  parameters are 400, names are length-checked. All SQL uses bind parameters
  (the only dynamic SQL is built from fixed fragments chosen by feature checks).
  Database names address a connection pool only after they were found in
  `pg_database`.
- **Error bodies** never contain driver messages, SQL or stack traces; internal
  causes are logged with the request id.
- **Static headers**: `nosniff`, `no-referrer`, `X-Frame-Options: DENY`, a CSP of
  `default-src 'self'`, `Cache-Control: no-store` on API responses. HSTS is not
  set (the server may speak plain HTTP on localhost).

## Contract notes

The 0.1.0 defects (missing 422/429/503/405 responses, no 405 code, no server
identity, "map group has id 1", unstored database settings answered with
invented values, free-form audit action names, the CSRF-refresh wording) are
resolved by revision 0.2.0 and are gone from this list. What remains, or is
new in 0.2.0 (the contract was not changed):

1. **Non-nullable fields without a data source.** `Server.version` (the
   KingresDB version of a remote server is unknown; every server reports this
   server's version), `Database.createdAt` (no catalog stores it; estimated
   from the database directory's `PG_VERSION` file, else the postmaster start
   time) and `Database.createdBy` (the owner, not the creator),
   `ClusterOverview.clusterId` and `.name` (the nil UUID and `""` for a node
   whose state file carries no cluster identity, while `Session.clusterId` and
   `.clusterName` are nullable).
2. **`LedgerStatus.mode` and `.digestIntervalSeconds` are not nullable** while
   `Database.ledger.mode` and `.digestIntervalSeconds` are. No catalog stores
   either, so `getLedgerStatus` reports `optIn` and 3600 (the only ledger mode
   the extension offers and the contract default) for a database with a ledger
   catalog, and `off`/3600 without one, while `getDatabase` says `null`.
3. **`notInUniqueKey` is ambiguous.** "A hash-distributed table needs a primary
   or unique key that includes the distribution column and this column is not
   part of one" can mean "no unique key contains it" or "some unique key lacks
   it". PostgreSQL needs the partition key in every unique index, so a column
   is blocked when any unique index lacks it.
4. **"Caller address not permitted while `localhostOnly`"** (403 on the
   setup-token operations) does not say which addresses are permitted. The
   server permits loopback addresses when it listens on loopback only
   (`localhostOnly`), and every address otherwise.
5. **`TableColumn.type`** has no stated format; the server returns
   `format_type()` of PostgreSQL (`integer`, `character varying(20)`).
6. The **`kingres.task`** table and the storage of the per-database ledger
   mode and digest interval are still not specified by the design notes
   (see "What is implemented").
7. **`Instance.dataDirectory` and `.version`** are optional and omitted: no
   catalog provides them. `ReplicationSettings.durability` is nullable in
   0.2.0, but `synchronous_commit` provides it, so it is returned.

---

Copyright (c) 2026, RooyeKhat Media Co. Released under the PostgreSQL License.
