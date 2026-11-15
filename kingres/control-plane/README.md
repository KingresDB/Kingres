<!-- SPDX-License-Identifier: PostgreSQL -->
# kingresd: the KingresDB control plane

`kingresd` is the control-plane server of KingresDB. It serves the management
REST API that the web panel and `kingres-cli` use
(`kingres/api/openapi.yaml`, conventions in `kingres/api/README.md`) and the
embedded web panel, on one port (default `127.0.0.1:8091`). It runs on every
server of a cluster.

This is the **third increment** (several servers). The second added the instance manager, the task engine, the
system database, cluster creation (`init-cluster`) and the operations behind the Databases, Security, Query and
Settings pages; the third lets a second and a third server join (`joinCluster`, `addServer`), removes an empty
server (`removeServer`) and makes the cluster-wide operations run on every server (see
[Running several servers](#running-several-servers)). Rebalance, failover, restoring preferred primaries
still answer the contract's "not supported yet" problem. The fourth step added server groups, leaving a cluster,
instance start and stop, local settings, inclusion proofs and verification records, security headers and
crash-injection tests of the task engine. The fifth step (contract 0.5.0) added certificate renewal and revocation,
the cluster-wide logs, alerts and metrics, the alert kinds that nothing raised, and the corrections listed in
"Fifth increment". Design: `kingres/docs/design/control-plane-2.md` and
`kingres/docs/design/control-plane-3.md`.

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
  sqlx/                   identifier quoting and the statement builder (the only way names reach SQL)
  scram/                  SCRAM-SHA-256 password verifiers
  sysdb/                  embedded migrations of the system database kingres_system (schema kingres_cp)
  instance/               instance manager: initdb, pg_hba.conf/pg_ident.conf/managed config rendering, pg_ctl
  task/                   task engine (leases, resumable steps) with PostgreSQL, file and memory stores
  cluster/                init-cluster, service credentials, proxy connections as signed-in roles
  pki/                    cluster certificate authority, server certificates, revocation list, CRL
  internalapi/             the server-to-server API on its own mutual-TLS listener (see internal-api.md)
  peerclient/             client of the internal API; forwarding to the leader
  peers/                  directory of the other servers' internal addresses (peers.json)
  membership/             joining and removing servers: the leader's tasks server.add and server.remove, and the
                          receiving side of the internal calls (instance, trust, certificates, globals, apply)
                          certificate renewal (the job of every server and the leader's side), revocation, the
                          version check, the view of the other servers for cluster-wide reads
  gather/                 the parallel call of the other servers with deadlines, the total orders and keyset
                          cursors of logs and alerts, the cluster-wide alert ids
  wire/                   request and answer types of those internal calls (no behavior)
  clustertest/            starts N kingresd processes on one machine; the multi-server tests
  leader/                 the manager leader read from the catalog, cached
  ops/                    operations that change the cluster (roles, grants, databases, tables, network, workbench)
  sdnotify/               systemd readiness notification
  pgtest/, contracttest/  test helpers (real instances; contract response validation)
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

Integration tests against real PostgreSQL instances run when `KINGRES_PG_BIN` is set.
`scripts/run-integration.ps1` builds the test binaries for Linux, copies them into WSL2 and runs them against
the installation in `~/kw-cp/install` (`scripts/run-wsl-tests.sh`); every instance is created by the instance
manager itself in a throwaway directory under `~/kw-cp/test`, on a slice of 200 ports of its own (20000-27999, see "Carry-overs"), and removed afterwards.

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
| `pg-bin-dir` | `KINGRES_PG_BIN_DIR` | none | directory of `postgres`, `initdb`, `pg_ctl`, `pg_config` (the package sets `/opt/kingres/bin`). Unset: the directory of the kingresd binary, then `PATH`; none found: instance management is off and `initCluster` answers 422. A directory that is not a KingresDB 18 build stops the server |
| `socket-dir` | `KINGRES_SOCKET_DIR` | `<data-dir>/run` | base directory of the instances' unix sockets (the package sets `/run/kingres`); never `/tmp` |
| `pg-port`, `pg-listen` | `KINGRES_PG_PORT`, `KINGRES_PG_LISTEN` | `5432`, `127.0.0.1` | port and `listen_addresses` of the managed instance. In a cluster of several servers `pg-listen` must include the address the other servers reach this one at (the `server-host`), and `127.0.0.1` for kingresd itself, for example `127.0.0.1,10.0.0.12` |
| `internal-listen` | `KINGRES_INTERNAL_LISTEN` | empty (off) | host:port of the mutual-TLS listener of the internal server-to-server API. **Required to join, add or remove servers and for forwarding.** By convention it is the port above the REST API (`listen` 8091, `internal-listen` 8092): the contract names only the REST address of a server, and the internal API of that server is found at the next port (kingresd warns when it is not) |
| `metrics-token` | `KINGRES_METRICS_TOKEN` | none | bearer token that may scrape `/metrics` besides stats-role session tokens |
| `server-host` | `KINGRES_SERVER_HOST` | machine host name | host registered in the shard catalog; it is the server id, the common name and a name of its certificate, and the host the other servers (and the sharding extension) connect to. It must resolve to this server from every other server |
| `certificate-lifetime` | `KINGRES_CERTIFICATE_LIFETIME` | `8760h` | validity of the server certificates the CA of this server signs (only the server that holds the CA key uses it); at least `5s`: short values exist so that tests can watch a renewal |
| `certificate-renew-fraction` | `KINGRES_CERTIFICATE_RENEW_FRACTION` | `0.3333` | a server renews its certificate when less than this part of its lifetime is left (above 0, at most 0.9) |
| `certificate-check-interval` | `KINGRES_CERTIFICATE_CHECK_INTERVAL` | `1h` | how often the renewal job looks (it also wakes when the renewal window of the certificate opens) |
| `certificate-retry-interval` | `KINGRES_CERTIFICATE_RETRY_INTERVAL` | `1m` | pause after a failed renewal; it doubles up to the check interval |
| `monitor-interval` | `KINGRES_MONITOR_INTERVAL` | `10s` | how often the health checks and the metric sampler run |
| `ledger-pending-max-age` | `KINGRES_LEDGER_PENDING_MAX_AGE` | `5m` | age of the oldest pending ledger transaction of a database that raises `ledgerPendingTooOld` |
| `version-check-interval` | `KINGRES_VERSION_CHECK_INTERVAL` | `1m` | how often the manager leader compares the versions of the servers (`versionMismatch`) |
| `gather-timeout` | `KINGRES_GATHER_TIMEOUT` | `3s` | the time one server gets to answer its part of a cluster-wide read of logs, alerts or metrics |
| `allow-remote-setup` | `KINGRES_ALLOW_REMOTE_SETUP` | `false` | `true` lets the setup operations come from non-loopback addresses while uninitialized (containers listening on `0.0.0.0`); the setup token is still required |
| `-version` | | | prints version, contract revision (0.7.0) and build commit (`-ldflags "-X main.commit=..."`, else `unknown`) |
| `-stop-instances` | | | stops the managed instances (fast shutdown) and exits |
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
bodies. The security headers and cookie attributes are described in
[Headers the server sends](#headers-the-server-sends).

## Headers the server sends

One middleware (`internal/httpapi/headers.go`) sets these on every response of
the listener: API success and error (problem+json), `index.html`, the SPA
fallback, hashed and other assets, 404, 405, the recovered 500, `/metrics`,
`/healthz` and `/readyz`. Tests: `TestSecurityHeadersEveryResponseClass`,
`TestHSTSIgnoresForwardedProto`, `TestCacheControlDefaultDoesNotOverride`
(`internal/httpapi/headers_test.go`).

| Header | Value |
|---|---|
| `Content-Security-Policy` | `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; font-src 'none'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'` |
| `X-Frame-Options` | `DENY` |
| `X-Content-Type-Options` | `nosniff` |
| `Referrer-Policy` | `no-referrer` |
| `Cross-Origin-Opener-Policy` | `same-origin` |
| `Cross-Origin-Resource-Policy` | `same-origin` |
| `Permissions-Policy` | `camera=(), microphone=(), geolocation=()` |
| `Strict-Transport-Security` | with TLS on (`tls-cert-file` set) only: `max-age=63072000; includeSubDomains`; never over plain HTTP; `X-Forwarded-Proto` is not trusted |
| `Cache-Control` | see below |

`Cache-Control` by response class:

| Class | Value |
|---|---|
| every `/api/v1/` response, errors, 404, 405, 500, `/metrics`, `/healthz`, `/readyz` | `no-store` |
| `index.html` and the SPA fallback (any non-API path without a file extension) | `no-cache` (as the panel specification says) |
| file under `/assets/` whose name has a content hash (`name-<8 base64url characters with a digit or an upper case letter>.ext`, as Vite writes it) | `public, max-age=31536000, immutable` |
| any other static file (`favicon.svg`, `manifest.webmanifest`, icons, a file under `/assets/` without a hash) | `no-cache` |
| a response whose handler set nothing | `no-store` (a handler's own value is never overridden) |

Decisions:

- The policy allows no inline script or style, no `data:`, no fonts, no
  external origin. `internal/webui/csp_test.go` scans the embedded panel
  (`index.html`, CSS) and a Vite-like fixture for inline scripts, `on*=`
  handlers, `javascript:` URLs, inline `<style>`, `style=` attributes, external
  origins and `data:` URLs in CSS. The placeholder page therefore uses
  `placeholder.css`, not an inline style. If the panel later needs inline
  `style=` attributes, `style-src-attr` must be added deliberately.
- `Cross-Origin-Opener-Policy: same-origin` isolates the browsing context group
  (no `window.opener` access from or to another site). `Cross-Origin-Resource-Policy:
  same-origin` stops other sites from embedding any response; Prometheus scrapes
  are server to server and unaffected.
- HSTS has no `preload`: submission to the preload list is practically
  irreversible and covers every subdomain of the host name, which an operator
  of an internal installation cannot promise. `includeSubDomains` stays.
- TLS is on when `tls-cert-file` is configured (`Deps.TLS`). A TLS-terminating
  proxy in front of a plain-HTTP kingresd is not detected: the proxy must add
  HSTS itself, and kingresd then uses the plain cookie name.

Session cookie (the panel never reads it: it is `HttpOnly`; the CSRF token comes
in the JSON body and lives in the panel's memory, so there is no CSRF cookie):

| | TLS off | TLS on |
|---|---|---|
| Name | `kingres_session` | `kingres_session` (no `__Host-` prefix: the panel specification fixes the plain name) |
| Path | `/` | `/` |
| Attributes | `HttpOnly; Secure; SameSite=Strict`, no `Domain`, `Max-Age` = session TTL | same |
| Logout | same name, Path and attributes with `Max-Age=-1` (empty value) | same |
| Accepted on requests | `kingres_session` | `kingres_session` |

`Secure` is set even without TLS, as the contract states: browsers accept it on
`localhost` and behind an HTTPS proxy. Tests: `TestSessionCookieAttributes`
(`internal/httpapi/cookies_test.go`). Nothing in `kingres/web` has to change for
the rename.

## What is implemented

The server implements revision **0.5.0** of the contract (88 operations, 71
paths; what is not done is in "Not done in this increment"). "Real" means the operation works against PostgreSQL or the local node
state. "Not yet" means: after authentication, CSRF and role checks have passed
the server answers **HTTP 422 with `code: unsupported_operation`**
(`Operation <id> is not supported by this server version yet.`), which every
authenticated operation lists in 0.2.0. **81 of 89 operations are real; 8 answer "not yet".**

| Tag | Real | Not yet |
|---|---|---|
| Alerts | `listAlerts`, `acknowledgeAlert` |  |
| Audit | `listAuditEvents` |  |
| Auth | `login`, `logout`, `getSession` |  |
| Cluster | `getCluster`, `getClusterSummary` |  |
| Roles | `listClusterRoles`, `listRoles`, `createRole`, `getRole`, `deleteRole`, `updateRole`, `setRoleClusterRoles`, `setRoleMemberships` |  |
| Databases | `getDatabaseCreateOptions`, `listDatabases`, `createDatabase`, `getDatabase`, `dropDatabase`, `updateDatabase` |  |
| Shards | `getBucketDistribution`, `listColocationGroups`, `listBuckets` |  |
| Ledger | `getLedgerStatus`, `listLedgerBlocks`, `listLedgerDigests`, `generateLedgerDigest`, `startLedgerVerification`, `listLedgerStatus`, `createInclusionProof`, `listLedgerVerifications`, `getLedgerVerification` |  |
| Tables | `getTable`, `distributeTable`, `enableTableLedger`, `listTables` |  |
| Grants | `listGrants`, `createGrant`, `revokeGrants`, `applyGrantTemplate` |  |
| Health | `getHealth`, `getReadiness` |  |
| Logs | `listLogs` |  |
| Metrics | `queryMetrics` |  |
| Query | `executeQuery`, `cancelStatement` |  |
| Rebalance |  | `getRebalanceStatus`, `startRebalance`, `previewRebalancePlan`, `listRebalanceReports`, `getRebalanceReport`, `stopRebalance` |
| Replication | `getReplicationTopology` | `restorePreferredPrimaries` |
| Server groups | `listServerGroups`, `createServerGroup`, `deleteServerGroup` |  |
| Servers | `listServers`, `getServer`, `addServer`, `removeServer`, `updateServer`, `renewServerCertificates`, `revokeServerCertificates` | `failoverServer` |
| Settings | `getClusterSettings`, `updateClusterSettings` |  |
| Network access | `getNetworkAccess`, `replaceNetworkAccess` |  |
| Setup | `initCluster`, `joinCluster`, `getNodeState`, `checkSetupToken` |  |
| Tasks | `listTasks`, `getTask`, `cancelTask` |  |
| This server | `getThisServer`, `listThisServerInstances`, `getThisServerInstance`, `restartThisServerInstance`, `listThisServerLogs`, `getThisServerResources`, `startThisServerInstance`, `stopThisServerInstance`, `leaveCluster`, `getThisServerSettings`, `updateThisServerSettings` |  |

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

## Operations

### Data directory layout

```
<data-dir>/
  node-state.json          node state (uninitialized, standalone, clustered), cluster id and name, setup task summary
  setup-token              one-time setup token (only while uninitialized, mode 0600)
  setup-task.json          the setup task (it must exist before the system database does)
  service-credentials      password of the role "kingresd" of the managed instance (mode 0600)
  audit.jsonl              audit log
  instances/<id>/          one directory per managed instance (id 1 on a one-server cluster)
    kingresd-instance.json   marker: kingresd created this directory; every operation checks it
    pgdata/                  PostgreSQL data directory
      kingres-managed.conf     managed settings, included by postgresql.conf (rewritten, do not edit)
      pg_hba.conf, pg_ident.conf  generated from the stored network rules, do not edit
  run/<id>/                unix socket of instance <id> (setting socket-dir; never /tmp)
```

### Start a one-server cluster by hand

```
kingresd -data-dir /var/lib/kingres -pg-bin-dir /opt/kingres/bin -listen 127.0.0.1:8091
cat /var/lib/kingres/setup-token
curl -s -X POST http://127.0.0.1:8091/api/v1/setup/init-cluster \
  -H "X-Setup-Token: $(cat /var/lib/kingres/setup-token)" -H 'Content-Type: application/json' \
  -d '{"clusterName":"prod","admin":{"username":"root_admin","password":"a-long-password"},
       "dataPaths":{"dataDirectory":"/var/lib/kingres"},"memoryQuota":{"totalMegabytes":4096}}'
curl -s http://127.0.0.1:8091/api/v1/setup/state      # follow setupTask until state is done
```

`dataPaths.dataDirectory` must be the data directory kingresd was started with; `walDirectory` and
`logDirectory` must be null (not supported yet; answered with 422 field errors).

### Recovering from an interrupted setup

The setup task is stored in `setup-task.json` and its steps are idempotent. If kingresd stops during setup it
resumes the task at the first step not done when it starts again, except that the step that needs the
administrator password cannot resume: the password exists only in the memory of the request that started the
task. In that case the task fails with `secret_unavailable` (or with the step's own error); call
`POST /setup/init-cluster` again with the same data directory and the setup token (the token stays valid until
setup finishes): the new task reuses the instance and the service credentials that already exist, creates what is
missing and alters the password of an administrator role that exists. To start over completely stop kingresd and
remove `<data-dir>/instances`, `service-credentials`, `setup-task.json` and `node-state.json`.

### What happens to the instances when kingresd stops

Instances are started with `pg_ctl start` and are independent processes: stopping or restarting kingresd leaves
them running, and kingresd at start-up re-adopts the instance it owns (marker file and `postmaster.pid`; starting
a running instance is a no-op). `kingresd -stop-instances` (same configuration) stops the managed instances with a
fast shutdown and exits. For the service unit: `KillMode=process`, `TimeoutStopSec=120`, and
`ExecStop=/opt/kingres/bin/kingresd -config <file> -stop-instances` to stop the database with the service.
`systemctl restart` then also restarts the database (ExecStop runs on restart); to restart only the manager send
it `SIGTERM` (`systemctl kill --kill-whom=main -s TERM kingres`) and let `Restart=` bring it back. kingresd sends
`READY=1` (after the API listens) and `STOPPING=1` to `NOTIFY_SOCKET` (standard library only, no-op when unset).

### Identity of the signed-in role

Role and grant changes and the query workbench run on a PostgreSQL connection of the signed-in role. kingresd
never keeps passwords, so PostgreSQL itself authenticates kingresd's operating system user over the unix socket
(peer authentication with `pg_ident.conf` map `kingres_proxy`): a role is mapped when it signs in with a valid
password, the map starts empty at every start, and the connections are ordinary sessions of that role. See
"Design notes".

### Running several servers

A cluster of several servers is formed by joining servers to the first one. Every server runs its own `kingresd`
with its own data directory and its own PostgreSQL instance; the first server (the **manager leader**) keeps the
cluster certificate authority, the system database `kingres_system` and the task engine.

**What every server needs** (the example is the server `db2.example.com`; the first server has the same
settings with its own names):

| Setting | Value | Why |
|---|---|---|
| `server-host` | `db2.example.com` | the server id: its certificate names it, the others and the sharding extension connect to it |
| `listen` | `0.0.0.0:8091` (or the address of the panel) | the REST API; the contract's `address` of a server |
| `internal-listen` | `0.0.0.0:8092` | the internal API, **the port above `listen`**: the contract gives only the REST address of a server and the internal API is found at the next port |
| `pg-port`, `pg-listen` | `5432`, `127.0.0.1,db2.example.com` | the PostgreSQL port and addresses; the other servers' instances connect here |
| `pg-bin-dir` | `/opt/kingres/bin` | as for a single server |
| `allow-remote-setup` | `false` | the setup operations stay local; adding a server does not need remote setup (the internal API takes the setup token) |

The ports to open between the servers: the internal API port (8092) and the PostgreSQL port (5432). The REST API
port is only for the panel and the clients. Every server name must resolve, from every other server, to the
address its instance listens on (a name that resolves to several addresses gets a certificate rule for each).

**Certificates.** Creating a cluster creates its certificate authority (ECDSA P-256, ten years) on the first server
in `<data-dir>/pki/` (`ca.key` mode 0600, never copied). Each server generates its own key, sends the CA only a
certificate request, and gets a one-year server certificate whose common name and subject alternative name are its
`server-host` (plus `localhost` and `127.0.0.1`). The same certificate is the server certificate and the client
certificate of the internal API and of the PostgreSQL instance of that server. `<data-dir>/pki/` of a server holds
`ca.crt`, `server.crt`, `server.key` (0600), `revoked.crl` and `revoked-serials.json`; the first server also `ca.key`
and `revoked.json`. Every server renews its own certificate before it expires (see "Certificates: renewal and
revocation"); the CA is not rotated.

**Joining a second server from its own API** (the wizard's way). On the new server, with the setup token it printed
in `<data-dir>/setup-token`:

```
curl -s -X POST http://127.0.0.1:8091/api/v1/setup/join-cluster \
  -H "X-Setup-Token: $(cat /var/lib/kingres/setup-token)" -H 'Content-Type: application/json' \
  -H 'X-Kingres-Pin-Sha256: <fingerprint of the existing server, optional>' \
  -d '{"existingNodeAddress":"db1.example.com:8091","admin":{"username":"root_admin","password":"..."},
       "dataPaths":{"dataDirectory":"/var/lib/kingres"},"memoryQuota":{"totalMegabytes":4096}}'
curl -s http://127.0.0.1:8091/api/v1/setup/state      # follow setupTask until it is done
```

**Adding a server from the cluster's API.** On any server of the cluster, signed in as `kingres_ops` or better, with
the new server's setup token:

```
curl -s -X POST http://db1.example.com:8091/api/v1/servers -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -H 'X-Kingres-Pin-Sha256: <fingerprint of the new server, optional>' \
  -d '{"address":"db3.example.com:8091","setupToken":"..."}'     # answers 202 with a task; follow GET /tasks/{id}
```

Both lead to the same task `server.add` on the leader (a request to a server that is not the leader is forwarded).
Its steps: wait for the new server to hold its certificate; prepare the instance on the new server (it creates
and starts its own instance with the cluster's settings and managed network rules); trust the new server on every
server; copy the roles (with password verifiers and memberships, never passwords) and every user database with the
extensions it uses; register the server with `kingres.add_node` in every database that uses `kingres_shard`
(this is the step after which nothing is cleaned up any more); record the server; finish (the new server becomes
`clustered`, its setup token is removed, the server directory is sent to every server). The server is listed with
status `pendingRebalance` until a rebalance (not written yet) moves buckets to it; databases created afterwards
spread their buckets over all servers at once.

**Pinned fingerprint or trust on first use.** (Since 0.6.0 a pin is required for `addServer` and `joinCluster` unless the request says `allowUnpinned: true`, and the fingerprint can be read from the server itself; see "Fix round for the second review" at the end. The text below describes the mechanism.) The first contact has no common CA. The internal API of a server
without a cluster presents a self-signed certificate (its fingerprint is logged at start, `bootstrap certificate of the
internal API`, and is the SHA-256 of `<data-dir>/pki/bootstrap.crt`); the internal API of a server of the cluster
presents the cluster certificate (`<data-dir>/pki/server.crt`). The contract 0.3.0 has no field for a fingerprint, so
the expected SHA-256 fingerprint (hex, colons allowed) goes in the header `X-Kingres-Pin-Sha256` of `addServer` (the
fingerprint of the new server's bootstrap certificate) or `joinCluster` (the fingerprint of the existing server's
internal certificate). With the header a different certificate is refused before the setup token or the password is
sent. Without it the fingerprint that was seen is recorded: in the log of the server that made the call, in the
task's detail, and in `kingres_cp.server.seen_fingerprint`; compare it with the other server's log afterwards.

**What each mode protects against.**

- *A network attacker between the two servers during a join without a pinned fingerprint* can present his own
  certificate and read the setup token (adding) or the administrator's password (joining), and can then act as
  that server's counterpart. Trust on first use gives no protection against an attacker who is in the path at that
  moment; it only makes a later comparison possible. Use the pinned mode, or join on a network you control.
  With a pin, the attacker cannot complete the handshake.
- *A malicious address given to the join* (an administrator is led to enter the address of a server of an
  attacker): without a pin the attacker receives the administrator's password (joining) or the new server's setup
  token (adding). The password of an administrator is the most valuable of these: **pin the fingerprint whenever
  the address was not typed from your own records**. When adding, the leader also checks that the id the new
  server calls itself resolves to the host it was reached at, so that it cannot register under another server's
  name, and refuses an id that already belongs to a member. A server that is added or joined gets a certificate
  only for the names its own id implies.
- *A stolen or replayed setup token*: the token is 192 random bits, valid only on its own server and only while
  that server is uninitialized, compared in constant time, rate limited per address, and removed when the server
  becomes clustered, so a captured token is useless afterwards. Before that, someone who holds the token and reaches
  the internal port can add the server to *a* cluster of his choosing (the leader he controls); keep the token
  secret and the internal port closed to untrusted networks until the server has joined.
- *A joined server calling internal operations it should not*: the internal API authenticates by certificate only and
  authorizes by a fixed table; a server that is not the leader cannot call the leader-only calls (instance, trust,
  certificates, globals, apply, reset, exec), and a removed server's certificate is revoked. **But every server
  of a cluster is fully trusted by every other**: a server can claim any identity in a forwarded call, and the
  PostgreSQL instances accept the certificate of any server as any database role (below). Someone who controls
  one server's key can act as any role on all of them. This is the design (D3), not a defect to be fixed by
  configuration.

**Trust between the PostgreSQL instances.** Every instance runs with `ssl = on` (TLS 1.3, the server certificate,
`ssl_ca_file` = the cluster CA, `ssl_crl_file` = the revocation list). Its generated `pg_hba.conf` starts with one
`hostssl all all <address of each server> cert map=kingres_nodes` rule per server (including itself and, for a cluster
on one machine, the loopback source address), and `pg_ident.conf` maps the certificate name of every server to any
role (`kingres_nodes "<server id>" all`). `kingres_shard.node_conninfo` in the managed configuration makes the
outgoing connections of the sharding extension present the server's certificate and verify the peer's against the
CA and its name (`sslmode=verify-full`). A statement that the sharding extension routes from one server to another
therefore runs there as the same role, authenticated by the certificate, with no password stored anywhere (the
role's password is not known to the cluster; only verifiers exist). Because those rules come first, **a
TLS connection from a server's own address that presents no certificate is refused instead of falling through to the
password rules**: clients on the servers themselves must use `sslmode=disable` (kingresd does) or present a
certificate. In plain words: every server of a cluster is fully trusted by every other.

**Removing a server.** `DELETE /servers/{serverId}` (kingres_ops) removes a server that owns no buckets in any
database: the state of a new server before a rebalance or after one drained it. The task `server.remove` checks
that again, unregisters the node (`kingres.remove_node`) in every database, removes the server from the managed
rules of the others, tells the server to stop and delete its instance and reset itself to `uninitialized` (it
deletes its key and certificates and gets a new setup token, so it can join again), revokes its certificate and
sends the revocation list to every kingresd and PostgreSQL instance, and forgets it. A server that owns buckets
(or holds a copy of them) is refused with 409 and a hint to rebalance first; the server of the map group (the
manager leader) is refused with 409 too; a server that does not exist is 404.

**Operations on every server.** Creating and dropping a database and replacing the network rules are tasks with
one step per other server ("Create the database on server X"). Role and grant changes keep their contract answer
(synchronous) and run on every server as well: they run on the leader as the signed-in role, then at once on every
other server (the role's current attributes, password verifier and memberships are read and copied; grants are
replayed with the service role, table and schema level grants only for databases without `kingres_shard`, whose
extension propagates them itself); a server that cannot be reached then gets a task of kind `replicate.apply`
that waits for it (see "Design notes"). A server that is down makes a task wait on that step, with the step
name naming the server (`currentStep`: "Create the database on server 10.0.0.13"), and it finishes when the server
is back; the server is never skipped.

**If a join is interrupted.** The task is resumable: when `kingresd` of the leader stops or crashes it takes up
at the first step that is not done after a restart (within the 30 s lease). A failure before the registration step
(for example the new server's port is taken) cleans up: the other servers stop trusting the new one, the new server
deletes its instance and key and becomes `uninitialized` again, and the certificate that was signed for it is
revoked; fix the cause and add it again. A failure during or after the registration is not undone (part of the
catalog may know the server); add the server again with the same setup token and the task continues. On the new
server, a join that was refused (wrong fingerprint, wrong credentials, the cluster discarded it) leaves
`setupTask.state = failed` in `GET /setup/state`; the setup token is still valid and `join-cluster` can be called
again. A pending server that has no unfinished task is revoked and forgotten by the leader's hourly check (and at start) once it is older than 15 minutes.

**Revoking a certificate.** The leader keeps the revoked serial numbers in `<data-dir>/pki/revoked.json`
(serial numbers in hex, with the time). Removing a server revokes every certificate that was issued to it, and
`revokeServerCertificates` revokes them without removing the server (see "Certificates: renewal and revocation").
The list is distributed immediately: the internal API of every server and, through `ssl_crl_file`, every PostgreSQL
instance refuse the certificate from then on (a reload is enough).

**Limits of this increment.** One map group (the first server) and one server per shard group: no standby, no
failover, no rebalance, no server groups. The map group's server cannot be removed or restarted away from, and
failover of the manager leader does not exist. A server must be reachable at the id it registers; DNS changes
need the trust rules to be applied again (any membership change does that). A cluster-wide task takes the list of servers when it starts: a server that joins while a
`database.create` or `database.drop` task is running is not part of that task (do not overlap them).

### Leaving a cluster

`POST /this-server/leave-cluster` (`kingres_admin`; optional body `{"force": false}`) is the mirror of
`removeServer`, driven from the server that leaves. The API of the leaving server does not forward it: it checks,
asks the manager leader over the internal API, and answers with the leader's task (`server.leave`, subject
`{type: server, id: <the leaving server>}`; 202 with `Location: /api/v1/tasks/{id}`).

**Refusals.** The same decision function (`internal/membership/refuse.go`) serves `removeServer` and `leaveCluster`,
with the contract's order: 404 for an unknown server; 422 `node_state_invalid` for a `dead` server (fail it over
first); 409 `last_server` (the only server of the cluster); 409 `server_hosts_map_group` (the server has an instance,
primary or standby, of shard group 0, or is the manager leader); 409 `server_owns_buckets` (a copy, primary or
replica, of any bucket of any sharded database; the `detail` names the databases with the number of buckets);
409 `rebalance_in_progress` (an unfinished `rebalance` task); 409 `operation_in_progress` (another unfinished task
of this server, such as a second leave or a `removeServer` of it). A leaving server decides first from **its own
copy of the catalog** (`kingres.node` and `kingres.bucket_map` in its local instance), then the leader decides again
from its own and starts the task; the server never asks the leader to remove another server (the call carries no
server id, see `internal-api.md`).

**`force`.** `force: true` lifts only the requirement that the rest of the cluster is reachable. The checks above
still run from the local copy, and a refusal is final (the leader leaving with `force` is still 409
`server_hosts_map_group`; a server with buckets still 409 `server_owns_buckets`). Without `force`, a leader that
cannot be reached (or is not known) is 503 `service_unavailable` and the server stays a member. With `force` and an
unreachable leader the server resets itself about a second after the answer: no task can exist on a cluster that
cannot be reached, so the answer is a finished task with `id` 0 (state `done`, one step) that cannot be fetched
with `GET /tasks/0`. The cluster, not knowing, keeps listing the server (its certificate is **not** revoked, since
only the leader can) until it is removed there with `removeServer` or failed over; this is the situation the
contract describes.

**The task on the leader.** Steps: check again and mark the server `removing`; unregister the node in every database
(`kingres.remove_node`); remove the server from the managed rules of the others; forget the server (its record and
its entry of the peer directory, which is sent to the others); revoke the certificate and send the revocation list to
every other kingresd and PostgreSQL instance. After the revocation the leaving server cannot call the leader again.

**What the leaving server does.** Unlike `removeServer`, the leader does not call the server to reset it. The leaving
server writes `<data-dir>/leave-pending.json` (task id and leader), then asks the leader every half second how
its task stands (`POST /internal/v1/leave-status`, answered only for the task whose subject is the caller). It
resets itself (the same reset as `removeServer`: stop and delete the instance and its credentials, delete the
certificates and key, forget the cluster, new setup token, peer directory reduced to itself) when the leader says
`done`, or when the leader refuses its certificate as revoked (that only happens in the last step; it then waits
four seconds for the leader to record the end). If the task fails the certificate is still valid: the server stays
a member and drops the record. If kingresd restarts while a leave is under way, it resumes the watch from the
record. The data directory handling is exactly that of `removeServer`: `instances/` (data, logs of the instance),
`service-credentials`, `pki/` are deleted; `audit.jsonl`, `node-state.json`, `peers.json` (reset to this server) and
kingresd's own log are kept. The server can join again with its new setup token (`joinCluster`) and gets a new
certificate.

**What a client sees.** The 202 and its `Location` are answered by the leaving server. While it is still clustered
and its certificate is valid, `GET /tasks/{id}` on the leaving server is forwarded to the leader like any
task read. The last step (revocation) ends that: the leader refuses the server's certificate, so the leaving server
answers 503 `service_unavailable` for about the next four seconds and then 503 `node_uninitialized` (it is
uninitialized; every API call except health and setup says so). The task is recorded `done` on the leader
immediately after the last step, which is before the server resets itself (the server waits for that); a client
should follow the task on the leaving server while that answers, and ask any other server (`GET /tasks/{id}` is
forwarded to the leader) when it answers 503. The task is not visible on the leaving server afterwards because the
server no longer belongs to the cluster.

**Audit.** The leaving server's pipeline audits `leaveCluster` (success or failure, `details.force`) in its own
`audit.jsonl`, which survives the reset. The leader records two `leaveCluster` events for the server
(`resourceId` is the leaving server, `source` is `system`, `details.phase` is `accepted` when the task starts and
`completed` after the last step).

**Not done.** A leave cannot be cancelled once the server is unregistered; a failed task after that point is not
undone (retry is by starting another leave, which the checks refuse once the server is no longer registered; use
`removeServer` or reset the server by hand). The leave of a server whose instance was stopped on purpose is not
tried.

## Fifth increment: certificates, cluster-wide reads, new alerts (contract 0.5.0)

Contract 0.5.0 is additive over 0.4.0 (see the changelog in `kingres/api/README.md`): two operations, optional
response properties, new enum members. Nothing a 0.4.0 client relies on changed, with the exception of two
corrections of the server to what 0.4.0 already said (below).

### Certificates: renewal and revocation

**The renewal job** (`internal/membership/certjob.go`, one per server, started when `internal-listen` is set). It
reads the server's own certificate (`<data-dir>/pki/server.crt`); when less than `certificate-renew-fraction` of
its lifetime is left (`pki.NeedsRenewalAt`) it renews: a follower sends `POST /internal/v1/pki/renew` with a
certificate request to the manager leader, which holds the CA key; the leader signs its own request without the
network. The request must be made from the key the server was admitted with and name only that server, so a server
renews itself and nobody else. The job looks every `certificate-check-interval` and also wakes when the renewal
window of the certificate opens, so a lifetime of seconds and one of a year work with the same code. A failure is
retried after `certificate-retry-interval`, the pause doubling up to the check interval. After three failures in a
row (`FailuresBeforeAlert`) the job raises the alert `certificateRenewalFailed` on its own server (severity
`warning`, `critical` when less than a tenth of the lifetime is left), clears it with the first success, and writes
two audit events in `audit.jsonl` of that server: `certificateRenewalFailed` (outcome failure) when a streak starts
and the same action with outcome success and `details.phase = "recovered"` when it ends. A renewal is audited on
the leader as `certificateRenewed` (actor `system`, `details.via = "renewal job"`, serial and `notAfter`).

**The revocation list is issued again by the same job (review finding CP2-04).** The list is valid for 30 days
(`pki.CRLValidity`) and PostgreSQL refuses one past its end, so it used to fail 30 days after the last add or remove.
The job of the server that holds the CA issues it again, and sends it to every server and instance (the call of a
revocation), when it is a third of that old (`pki.CRLRefreshAfter`, 10 days); a failure is tried again at the next
round (every `certificate-check-interval`, an hour by default), long before the end. Test:
`TestRenewalJobIssuesTheRevocationListAgainBeforeItEnds` (injected clock, 100 days, failures, a server without the CA).

**Decision: the key stays.** A renewed certificate is made for the key the server holds, with the same names. A new
key would have to be installed together with its certificate (two files, read by PostgreSQL, by the internal API
and by the sharding extension at different moments), which cannot be made atomic with plain files; with the key
kept, the files, `ssl_cert_file`, `pg_hba.conf` (`hostssl ... cert map=kingres_nodes`) and `pg_ident.conf` (which
maps the common name) stay valid as they are. The old certificate stays valid until it expires (it has the same
key); the CA records every certificate it issued (`kingres_cp.certificate`) so that a revocation reaches all of
them. A new key is a new join.

**What a renewal needs, part by part** (nothing is restarted anywhere):

| Part | How it takes the new certificate |
|---|---|
| internal API listener (server certificate) and its client side (kingresd's calls to the others) | read from `server.crt` at every handshake; the server then closes its idle connections to the others (a connection opened with the old certificate is checked against its end at every request) |
| the PostgreSQL instance (`ssl_cert_file`, `ssl_ca_file`, `ssl_crl_file`) | **a reload** (`pg_ctl reload`) of the running instance; an instance that is stopped reads the files when it starts |
| outgoing connections of the sharding extension (`kingres_shard.node_conninfo` names the files) | nothing: libpq reads the files at every new connection; sessions that exist keep working |
| kingresd's connections to other instances (`NodeTLSConfig`) | nothing: built at every connection |
| the public REST/panel listener (`tls-cert-file`) | not touched: it is the operator's certificate, not the cluster's |

**`renewServerCertificates`** (`kingres_admin`, task `certificate.renew`, subject the server): the leader asks the
server for a request (`GET /pki/csr`), signs it, records it and sends the certificate with `POST /pki/install`; it
also renews the leader's own certificate. It is the way to renew a certificate whose job cannot (an expired one
cannot call the leader; the leader can still call it). **`revokeServerCertificates`** (`kingres_admin`, task
`certificate.revoke`): revokes every certificate that may still be valid, records `certificateRevokedAt` of the
server (shown in `Server`), sends the revocation list to every server (the task waits for a server that is down,
like every cluster-wide task) and drops the connections to the server. *What stops working*: the internal API of
every server refuses the revoked server (its job, forwarding, task recording, leave), the PostgreSQL instances
refuse its certificate (`hostssl ... cert`, in both directions of the sharding connections) and kingresd refuses to
connect to it; connections that were open before keep running until they end. *How the server gets back in*: it
never gets a new certificate (`renewServerCertificates` is 409 `certificate_revoked`); a server that owns no bucket
is removed with `removeServer` (the leader still reaches it for the revocation list and for `/node/reset`: the
revocation list is not consulted for the calls `peerclient.DoUnchecked` makes) and added again with a new setup
token, key and certificate. A server that owns buckets cannot be removed before a rebalance has emptied it,
which this version cannot do (**limit**): revoking the certificates of a server with data is a one-way door until
rebalance exists. The leader's own certificates cannot be revoked (409 `server_hosts_map_group`). A revoked server
can leave on its own with `force` (it cannot ask the leader); the cluster keeps listing it until `removeServer`,
which does not wait for a server that has already reset itself.

**The CA key and the manager.** The key lives in `<data-dir>/pki/ca.key` of the server that created the cluster. A
manager leader that does not hold it (after a change of the manager, which does not exist yet) answers
`renewServerCertificates`, `revokeServerCertificates`, `addServer`, `joinCluster`, `removeServer` and
`leaveCluster` with **503 `certificate_authority_unavailable`** (a problem code of 0.5.0, detail naming the leader
and the way out) before anything is changed, and the internal call of the renewal job with 503 `ca_unavailable`; the
job counts it as a failure, so the alert `certificateRenewalFailed` and, as the certificate nears its end,
`certificateExpiring` make the situation visible. The cluster keeps running on its certificates until they expire.

Tests: `internal/pki` (`TestShortLifetimeAndRenewalFraction`, `TestRemainingFraction`); `internal/config`
(`TestCertificateSettings`); the job's logic with a fake clock (`internal/membership/certjob_test.go`:
`TestRenewalJob...`); the CA-less leader (`TestOperationsThatNeedTheCAAnswerADedicatedProblemWithoutIt`);
`internal/peerclient/revoked_test.go`; handlers (`internal/httpapi/handlers_certs_test.go`); and, with real
processes and certificates that live 24 to 60 seconds (`internal/clustertest/certs_test.go`):
`TestCertificateRenewalKeepsAThreeServerClusterWorking` (three servers, statements routed between them over fresh
connections every 300 ms for 60 s, a session kept open through every renewal, at least two renewals per server,
PostgreSQL not restarted, the instance presents the certificate that is in the file, the renewals audited),
`TestRevokedCertificateIsRefusedAndTheServerGetsBackIn` (a revoked certificate refused by the internal API and by
PostgreSQL, a server cannot renew with another key or for another server, an administrator cannot renew a revoked
server, remove and add again), `TestRevokedServerLeavesWithForceOnly`,
`TestCertificateAuthorityMissingOnTheLeader` (the dedicated problem on every operation, the job's retries, the two
alerts raised and cleared, the audit of the streak).

### Cluster-wide logs, alerts and metrics

`listLogs`, `listAlerts` and `queryMetrics` answer for the whole cluster from any server (`internal/httpapi/gatherhttp.go`,
`internal/gather`, `POST /internal/v1/gather`; the wire is specified in `internal-api.md`). The receiving server
checks the session and the role as for any request, asks every other server in parallel with its own deadline
(`gather-timeout`), merges the answers and names the servers that did not answer: `partial: true` and
`missingServers[]` with the reason `timeout`, `unreachable`, `refused` or `invalidAnswer` (optional properties of the
three page types in 0.5.0; 0.4.0 had no way to say it). **Role checks are made again on every server that answers**
(the forwarded-identity rule of CP-07: the answering server derives the caller's cluster roles from its own catalog).
**Sizes are bounded**: a server answers at most one more entry than the page needs (alerts, logs) and at most 16 MiB;
a metric answer has at most 2000 series and exactly the points asked for; a log message is cut at 8 KiB; text of
another server passes `peerclient.CleanText`; a server can only speak for itself (its id replaces whatever its
log entries say, an alert must carry its ordinal, series labels are its own).

**Cursors are stable across servers.** They are keyset cursors over one total order: logs by time, then server id,
then the rank among the entries of that server with the same second; alerts by time raised, then id. Every server
is asked for what comes after the cursor, so a page boundary is the same whichever servers answered, and a server
that was down for one page contributes to the next. The cursors of 0.4.0 (offsets, ids) are refused with 400.

**Alerts of every server.** Each server keeps the alerts about itself in its own `kingres_cp.alert` (a follower's
system database now has the `kingres_cp` schema too: `MigrateSystem` runs at every start and after a join, which
also upgrades the leader's schema when a binary brings a new migration); the alerts about the cluster as a whole
(`versionMismatch`) are kept by the manager leader. An alert id is `ordinal << 32 | local id`; the ordinal is the
number `kingres_cp.server.ordinal` gave the server (migration 4; sent to every server in `peers.json`, a leader
sends the directory again at start so that an older cluster learns the ordinals). The id is unique, below 2^53 (a
browser reads JSON numbers as doubles) and names the server that holds the alert, so `acknowledgeAlert` is no longer
forwarded to the leader: the receiving server acknowledges it itself or asks the holder, which audits it once. A
server that is not reachable when its alert is acknowledged is 503; an unknown ordinal is 404. `listAlerts` is no
longer a leader-bound read.

`listLogs` also honours what the contract always said and the server ignored: `level` keeps the entries at that level
or more severe (it was an exact match) and `q` filters the message (case-insensitive). `listThisServerLogs` uses the
same code for its own server.

Tests: `internal/gather`; `internal/httpapi/gatherhttp_test.go` (three in-process servers: merged order, paging
from different servers, filters, unreachable, slow and refusing servers named within the deadline, the role checked
again, cleaned answers, metric sums per server and per database); `internal/monitor` (`TestAssembleOfPerServerSeriesEqualsOneRing`,
`TestAlertSubjectsAndKeysetListing`); `internal/clustertest/gather_test.go` (a real three-server cluster:
alerts of every server from every server with unique ids, paging by one, acknowledgement of an alert of another
server with one audit event on its holder, logs of all servers newest first, one series per server, a role that the
answering server refuses named as `refused`, a stopped server named).

### Alerts raised since 0.5.0

Each alert has a subject (`server:<id>`, optionally `|database:<name>` or `|ca`, or `cluster`); there is one open
alert per kind and subject; a condition that holds again updates the open alert (severity, message) and never raises
a second one; a kind whose reading was not measured is neither raised nor cleared.

| Kind | Raised when | Cleared when | Where | Test |
|---|---|---|---|---|
| `certificateExpiring` | the server's own certificate has less than half of the part of its lifetime at which it is renewed left (the job has not managed to): `warning`; less than an eighth or expired: `critical`. On the server that holds the CA: the CA certificate has less than a year (`warning`) or 30 days (`critical`) left (the CA is not rotated) | the certificate is younger again (renewed) | every server, for itself; every `monitor-interval` | `TestEvaluateCertificateExpiring`; `TestCertificateAuthorityMissingOnTheLeader` (raised and cleared with real certificates) |
| `certificateRenewalFailed` | three renewals in a row failed (see above) | the next renewal succeeded, or the certificate was renewed by other means | the server whose job it is | `TestRenewalJob...`; `TestCertificateAuthorityMissingOnTheLeader` |
| `ledgerPendingTooOld` | the oldest pending ledger transaction of a database (`kingres.ledger_pending`) has waited `ledger-pending-max-age` (default 5 min; `critical` at twelve times that). The extension records no time for a pending entry, so the monitor remembers the lowest pending transaction number and when it first saw it (`monitor.PendingTracker`); when another entry becomes the oldest the age starts again (an under-estimate), and a restart of kingresd forgets it | the queue of that database is empty or its oldest entry changed | every server, per database, every third monitor round | `TestPendingTrackerCountsTheAgeOfTheOldestPendingEntry`, `TestEvaluateLedgerPendingTooOld`; `TestLedgerPendingTooOldAlert` (block builder switched off and on in a real instance) |
| `versionMismatch` | the servers that answer `GET /internal/v1/state` report different KingresDB (kingresd) versions, PostgreSQL versions, or versions of `kingres_shard` / `kingres_ledger` (the `default_version` their instance offers); message lists what differs and on which servers | the servers agree; with fewer than two answers nothing changes | the manager leader, subject `cluster`, every `version-check-interval` | `TestEvaluateVersions`, `TestVersionMismatchAlertLifecycle` (raised once through repeated checks, unchanged while a server does not answer, cleared). Not tested with two real binaries of different versions: the test environment has one |

Limit: an alert is kept by the server that raised it, so after a change of the manager leader (not implemented) the
cluster-wide alerts of the old leader stay open there until it evaluates them again.

### Listings with a hard cap

`listTables`, `listRoles` and `listGrants` no longer read the whole result: the
database builds one row more than the page needs, with keyset paging in SQL (cursors are opaque in the contract, so
this needed no contract change): tables by (schema, name) with the class filter in the same query; roles by name;
grants by (database, level, schema, table, grantee), per database and per level, with the grouping of the
privileges in SQL. The cursors of 0.4.0 (offsets) are refused with 400. Test:
`TestIntegrationKeysetPagingOfTablesRolesAndGrants` and the existing listing tests of `internal/store/pgstore`.
The **bucket list** (`listBuckets`) still reads every bucket of a colocation group and pages in memory (at most the
bucket count, 4096); `listDatabases` and the other small lists are unpaged by contract.

### Corrections and decisions about the contract's texts

| Item | Decision |
|---|---|
| Audit event of a forwarded request lost `forwardedBy` and `originRequestId` when a handler set `c.details` (add-server, join, leave, the query workbench, local settings) | fixed centrally: the audit writer adds the two facts after whatever the handler put in; `TestForwardedAuditKeepsForwardedByWhenTheHandlerSetsDetails` |
| `leaveCluster` said the server returns to `standalone` | the server returns to `uninitialized` (instance and key deleted, new setup token); the contract text was wrong and is corrected in 0.5.0 |
| no wording for the forced leave with a leader that cannot be reached | the contract now says: 202 with a `Task` of `id` 0, `state: done`, one step, not fetchable; and that a leader that refuses the revoked certificate of the server is the same situation (the server used to answer 503 whatever `force` said) |
| unknown members of a request body: 400 `bad_request`, the contract said `validation_failed` | for the three requests the contract declares closed (`ServerUpdateRequest`, `LocalSettingsUpdate`, `InclusionProofRequest`) the answer is `validation_failed` with `errors[].code = unknown_property` (only `createInclusionProof` was wrong; its end-to-end test said `bad_request` and was corrected); for every other request body an unknown member stays `bad_request` and the contract says so (the `BadRequest` response) |
| `removeServer` of a server that had reset itself (a forced leave) waited for it for ever | the leader treats a server that no longer presents a certificate of the cluster (`peerclient.ErrNotMember`) as already reset |

### What rebalance and failover orchestration will need from this code

- **The CA key and the revocation list move with the manager.** `<data-dir>/pki/ca.key`, `revoked.json`, the table
  `kingres_cp.certificate` (every issued serial) and the columns `kingres_cp.server.certificate_*` /
  `ordinal` are the state a new manager leader needs; until the key moves, every operation in the list above
  answers `certificate_authority_unavailable` and the renewal jobs of the other servers fail visibly (three failures,
  then the alert; a certificate that expires meanwhile can only be replaced by the leader's call
  `renewServerCertificates`, so the key must move before the first certificate runs out: a third of a year by default).
- **Use the existing calls for a server that is cut off.** `peerclient.Client.DoUnchecked` reaches a server whose
  certificate is revoked (revocation list, reset); everything else is refused for it on purpose. A failover that
  fences a server by revoking its certificates will not be able to make it renew, forward or record tasks.
- **The peer directory carries ordinals** (`peers.Peer.Ordinal`); alerts of a failed-over manager stay with the
  server that holds them (`gather.AlertID`); `Gatherer.Others` asks the records of the leader, the others ask the
  directory.
- **Cluster-wide reads tolerate a missing server** (`missingServers`): a rebalance or failover report should use the
  same fan-out (`gather.Fanout`) with a deadline per server and report partial answers instead of failing.
- **A step that must reach every server** uses `waitUnreachable` of the membership code (the task waits and says so
  in `Task.detail`); a step that must not wait for a revoked or reset server uses `DoUnchecked` and treats
  `ErrNotMember` as done (see `stepRemoveReset`).
- **Server groups and `shouldHaveShards`** are still only records; the revocation of a server's certificates with
  buckets (above) is the first operation that needs a rebalance to finish properly.

## Security review of 2026-11-14 (CP-01 to CP-07) and what the logs can contain

The review `docs/reviews/2026-11-14-kingresd-control-plane.md` was fixed before anything else in this increment. Each
finding has a test that fails on the reviewed code.

| Finding | Reproducing test | Fix |
|---|---|---|
| CP-01 (critical) a database name from a path is spliced into the service DSN | `TestServiceDSNDatabaseNameCannotRedirect` (`internal/cluster`; failed before: the name added `host` and `port`); `TestCP01DatabaseNameCannotRedirectTheServiceConnection` (WSL: a listener named in the database name gets no connection) | no connection string is built by formatting: `internal/pgconninfo` is the only place that quotes values per libpq's rules (NUL refused, keywords from a closed list); used by the service DSN, `kingres_shard.node_conninfo`, the fallback DSN and the test helpers; `TestNoConnectionStringsAssembledElsewhere` scans every Go file of the module; `FuzzFormatRoundTrip`; internal-API URLs are built by `internalapi.URL` and the forwarded request URL as a `url.URL` value. Service connections for operations use the instance socket (the read pools and the sign-in use loopback TCP) |
| CP-02 (critical) sessions keep the roles of sign-in | `TestSessionsFollowTheCurrentStateOfTheRole`, `TestRoleChangedEndsItsSessionsAtOnce`, `TestLeaderChecksForwardedIdentityAgainstTheRole` (unit); `TestCP02SessionsFollowTheCurrentRole` (WSL: the review's scenario, a role disabled in SQL, a dropped role, a password change) | every request re-derives the role's cluster roles from `pg_roles` and `pg_auth_members` (`auth.Revalidator`): a role that no longer exists, cannot log in, has expired or has no cluster role gets 401 and loses its sessions. The answer is cached for **3 seconds** per role, so a change made directly in SQL is seen within 3 s. A change through the API (create, update, delete, memberships, cluster roles) ends the role's sessions at once on this server and, through the internal call `POST /apply` with kind `role.changed`, on every other server, and removes the role's entry of the acting-as map. A backend that cannot be asked fails closed (503) |
| CP-03 (high) statement text in PostgreSQL's log, served by `/logs` | `TestCP03FailedStatementTextIsNotLogged` (WSL) | the managed configuration sets `log_min_error_statement = panic`, `log_statement = none`, `log_min_duration_statement = -1` and no parameter logging, and local settings cannot override them; `listLogs` and `listThisServerLogs` need **kingres_ops** (contract 0.4.0, breaking) |
| CP-04 (high) no size cap on workbench results | `TestCP04WorkbenchResultsAreBounded` (WSL; shows the allocation of the review's statement) | result budget 16 MiB (then `truncated: true`), one value at most 1 MiB (422 otherwise), at most 4 statements at once per server (503 with `Retry-After` otherwise); the internal `exec` call has the same caps. Peak memory is the budget plus one row as PostgreSQL sends it |
| CP-05 (medium) dangerous predefined roles can be granted | `TestCP05DangerousMembershipsAreRefused` (WSL) | memberships in any `pg_*` role except the harmless monitoring ones, and in roles with SUPERUSER, REPLICATION or BYPASSRLS (or that lead to a dangerous predefined role), are refused with 400; the workbench answers 403 for a role that has such an attribute or a membership in `pg_execute_server_program`, `pg_read_server_files`, `pg_write_server_files` or `pg_read_server_logs`. A superuser can still grant these in SQL; that is outside the panel's model |
| CP-06 (low) control characters in user names | `TestLoginRefusesControlCharactersInTheUserName`, `TestCleanReplacesControlCharacters` | `login` refuses control characters (400) before PostgreSQL and the audit log; the audit sink replaces control characters in actor and resource id |
| CP-07 (medium) the leader trusts a forwarded identity | `TestLeaderChecksForwardedIdentityAgainstTheRole` | the leader derives the claimed role's cluster roles itself (the check of CP-02) and refuses an identity that does not hold the claimed roles; a forwarding server must be in the peer directory (its certificate is already checked against the CA and the revocation list). The stated trust model is unchanged: every server of a cluster is fully trusted |

### Class search of 2026-11-14 (leftovers)

The same classes as CP-01 to CP-07 were searched in the code the fixes did not touch. Fixed (each with a test that
failed before): the `/metrics` scrape decided a bearer token's privilege from the roles cached at sign-in (CP-02 class;
`TestMetricsScrapeFollowsTheCurrentStateOfTheRole`), now re-derived like every other request; the error text a peer
answers with was quoted unbounded and unescaped into problems and task errors (CP-06 class;
`TestPeerErrorTextIsBoundedAndClean`), now cut at 500 bytes with control characters replaced; the managed
configuration accepted `include_dir`, `include_if_exists`, `archive_command` and other file/command settings in its
`Extra` map (`TestManagedExtraSettingsCannotIncludeOrRedirectFiles`), now refused. Checked and sound: every SQL
statement is built by `sqlx.Builder` (identifiers quoted, literals quoted, fixed text restricted to a closed alphabet)
or bound with `$n`; the one `Fixed(k)` built from stored data (`ALTER DATABASE ... SET`) is guarded by `gucRE`;
pg_hba.conf, pg_ident.conf and postgresql.conf values are quoted or refused when they hold control characters; every
request body is read through `http.MaxBytesReader` (REST 1 MiB default, internal API 1 to 16 MiB) and every peer answer
through `io.LimitReader`; every log call goes through `slog`, which quotes values, and the audit sink cleans actor,
address and resource id. Fixed afterwards: catalog listings (`listTables`, `listRoles`, `listGrants`) read every row of the
catalog and paged in memory, so their size followed the number of tables or roles that the database's own users can
create; they now page with a keyset in SQL (see "Listings with a hard cap").

**What the logs can contain.** The text of statements is not written to the log of a managed instance (settings above;
an operator who overrides them in `postgresql.conf` after the include of the managed file can undo this). The error
message and the DETAIL line that PostgreSQL writes can still contain data values (a unique violation names the key
value), which is why the log operations need `kingres_ops`. The kingresd log and the audit file never contain
statement text, passwords or tokens.

**Blast radius of the service password.** The service role (`kingresd`, a superuser) authenticates with a SCRAM
password stored in `<data-dir>/service-credentials` (0600). The service connections kingresd opens for operations use the
instance's unix socket, so the password is not sent over TCP there, and no connection string can be redirected (CP-01);
the read-side pools and the sign-in authenticator still use loopback TCP (the socket has only a peer rule for other
roles), and the loopback password rule remains in `pg_hba.conf`. Peer authentication for the service role was
not adopted. A `pg-dsn` of an unmanaged instance is used as the operator gave it. Connections to other servers use
the server certificate and no password.

**Connecting locally as an administrator.** The supported way is through kingresd (the query workbench). A DBA with
the operating system account that runs kingresd can use `psql -h <socket-dir>/<instance id> -p <pg-port> -U <role>
postgres` (the package's socket directory is `/run/kingres/1`) with the role's password. `psql` does not find the
socket without `-h` because the sockets are not in PostgreSQL's default directory.

**Socket directory.** The socket directory of an instance (`<socket-dir>/<id>`, mode 0700) is created before every
`pg_ctl start`, not only when the instance is created (`TestStartRecreatesTheSocketDirectory`): after a reboot, when
`/run` is empty, the start works, and a package no longer needs `RuntimeDirectory=kingres kingres/1` for the instance
directory.

### Task engine: crash points and guarantees

A task is a list of steps. Before it calls a step the engine records the step as `running` in `kingres_cp.task`, after
the step returned it records `done`, and a lease (renewed every third of its length) says which process owns the task.
When that process dies, the next engine that polls takes the task over after the lease expired and starts at the
first step that is not recorded `done`. What the engine promises:

- A step recorded `done` never runs again.
- A step that ran but whose `done` was not recorded runs again (**at least once**). Every step must therefore be
  idempotent unless it is marked `NonIdempotent`.
- A `NonIdempotent` step that was recorded `running` but not `done` is **not** run again: the task fails with the
  code `step_interrupted` (not retryable) and names the step, because it is unknown whether the effect happened. The
  steps after it do not run. A crash before the `running` record leaves the step untouched, so it runs once.
- A task whose process died is never lost and never stays `running` forever: once the lease expired it is resumed or
  failed as above, as long as some server runs the engine. A task that has all steps `done` is recorded `done`.
- Secrets live only in the process that started the task; a resumed step that needs one fails with
  `secret_unavailable`.

Crash points (`task.FaultPoints`, set through `task.Options.Fault`, nil in the product): `before_step`,
`step_started` (recorded running, `Run` not called), `step_ran` (`Run` returned, `done` not recorded),
`step_recorded`, `lease_renew` (a renewal tick while a step runs), `before_finish` and `after_finish`. Tests make the
hook end the process: `crash_test.go` simulates the death with a store that refuses all writes after the hook ran
(`TestCrashAtEveryStepBoundary`, `TestCrashAtTheFinish`, `TestCrashAtTheLeaseRenewal`), and `pgcrash_test.go`
(`TestTaskEngineSurvivesAKilledProcess`, real PostgreSQL, WSL) starts the test binary as helper processes that
`os.Exit(137)` at each point, then a second process takes the task over; the steps count their runs in a side file.
The real task `server.add` is killed with SIGKILL at each of its steps by `TestJoinInterruptedAtEveryStepResumes`.
No task kind of the product is marked `NonIdempotent` today (all steps check what exists before they create).

## Not done in this increment, and the manager's state on a leader change

**Not done** (each still answers 422 `unsupported_operation`, or is unchanged): rebalance, failover and restoring
preferred primaries (8 operations answer "not yet"), a way back for a server with buckets whose certificates were
revoked (it needs rebalance), the rotation of the CA itself, and any change of the manager leader (the three bullets
below). Certificate renewal and revocation, the dedicated problem for a leader without the CA key, cluster-wide
logs, alerts and metrics and the four alert kinds that nothing raised are done (see "Fifth increment"). Server groups, `updateServer`, inclusion proofs, ledger verification records, instance start and
stop, leave-cluster and local settings are done (see their sections).

**What has to move with the manager (for the high-availability increment).** The certificate authority key
(`<data-dir>/pki/ca.key`), the revocation list (`revoked.json`), the task engine (rows of `kingres_system` on the
leader's instance), the directory of internal addresses (`peers.json`, on every server), the audit file
(`audit.jsonl`, per server) and the session store (memory, per server) belong to the first server today. A leader
change (not implemented) has to move the CA key and the revocation list (encrypted, to the standby leader) and the
`kingres_cp` schema (it lives in the map group and follows it); followers already re-read the leader from the catalog
every 2 s. Until then the limitation is: **if the manager leader is not the server that holds the CA, operations that
need the CA (adding a server, removing one, certificate operations) cannot work**; the cluster keeps running on its
existing certificates until they expire (one year). See `docs/design/ha-phase5.md`.

## Design notes (where the specification was silent or was changed)

1. **Role connections (D6)**: the spec keeps the sign-in connection. A connection is bound to one database and the
   workbench takes a `database`, so a kept sign-in connection cannot serve it; the peer-authenticated proxy above
   serves every database without storing a password. Role creation, update, drop and all grants/revokes run as the
   signed-in role (PostgreSQL's checks apply). Exceptions, all authorized first by the cluster-role check:
   membership changes (cluster roles and ordinary roles) run with the service role, because PostgreSQL lets a role
   revoke only the grants it made itself and a second administrator could otherwise never undo them; changes to a
   role that is itself an administrator run with the service role, because the ADMIN OPTION that would let
   administrators manage each other is a membership cycle. Roles created through the API are granted to
   `kingres_admin` WITH ADMIN OPTION, INHERIT FALSE, SET FALSE so that every administrator can manage them.
   Membership walks consider only rows with `inherit_option` (PostgreSQL 16+ creates such implicit rows).
2. **Passwords** reach PostgreSQL as SCRAM-SHA-256 verifiers computed in kingresd (package `scram`), so the clear
   text is never in a statement, in `pg_stat_activity` or in the PostgreSQL log. `VALID UNTIL` "no expiry" is
   `infinity`; the read side maps it to null.
3. **Setup task storage**: the system database does not exist during setup, so the setup task lives in
   `setup-task.json` and is copied into `kingres_cp.task` (same id) when it is done; later tasks are rows of
   `kingres_cp.task` executed by the engine.
4. **Tasks that finish quickly are still tasks** (202): createDatabase, dropDatabase, distributeTable,
   replaceNetworkAccess. updateDatabase, enableTableLedger, roles, grants and the workbench are synchronous.
5. **Database-wide ledger mode**: `off` and `optIn` only; `appendOnly` and `updatable` as a database mode answer 422
   `unsupported_operation` (per table: `enableTableLedger`; `updatable` needs `kingres_ledger` 0.2).
6. **Reference tables**: `kingres_shard` 0.1 has no function for them: `method: reference` answers 422
   `unsupported_operation`.
7. **Network rules** are stored by revision in `kingres_cp.network_rule`; the generated `pg_hba.conf` always starts
   with fixed rules for the role `kingresd` (loopback, SCRAM) and the peer rule, so managed rules cannot lock the
   control plane out. `trust` does not exist as a method. A rule set under which the caller's own address would be
   rejected (first match) is refused with 422. A reload takes effect within a moment of the task finishing.
8. **The workbench** uses the extended protocol (so a text with several statements is refused by PostgreSQL), sets
   `statement_timeout` with `set_config` and a bind parameter, commits on success, and stops reading at `rowLimit`
   rows by canceling the connection. Its audit event holds the database, the length and a hash prefix of the
   statement, never the text.
9. **`pg-bin-dir` check**: this KingresDB build's programs print `PostgreSQL 18.x`, not a KingresDB string, so
   start-up requires `postgres`, `initdb`, `pg_ctl` (version 18) and `pg_config`, and that `kingres_shard.so` and
   `kingres_ledger.so` exist in `pg_config --pkglibdir`; a plain PostgreSQL is refused.
10. **Setup from remote addresses** is controlled by `allow-remote-setup` (default false), no longer by the listen
    address: the setup operations are accepted only from loopback callers unless it is true (containers).

11. **Where the internal API of a server is** (third increment). `joinCluster` and `addServer` give the REST address of
    a server; the contract has no field for the internal one. The internal API of a server is at its REST host and
    the port above its REST port. kingresd warns at start when `internal-listen` is not on that port.
12. **First contact on the internal listener.** The internal listener also answers three calls without a client
    certificate (`/internal/v1/bootstrap/prepare`, `install`, `join`), authenticated by the new server's setup token
    or an administrator's credentials, over TLS 1.3 with a self-signed certificate whose fingerprint is pinned or
    recorded (see "Running several servers"). The handshake therefore asks for, but does not require, a client
    certificate; every other call still needs a valid one and answers 401 `no_certificate` otherwise. The public
    listener is unchanged and never takes a client certificate.
13. **The fingerprint has no contract field** in 0.3.0: it is the header `X-Kingres-Pin-Sha256` of `joinCluster` and
    `addServer` (forwarded with the request), and the seen fingerprint is in the log, in the `detail` of the task
    (`Task.Detail`, stored in the task's progress, not part of the contract) and in `kingres_cp.server`. Contract
    0.4.0 should carry both.
14. **Introduction happens before the task.** `POST /servers` contacts the new server, signs its certificate request
    and records it as `pending` in one transaction, installs the certificate on it and only then submits the task,
    whose subject is the server id (known only after the first contact). The task's first step therefore waits for
    the new server to answer with its certificate instead of introducing it. A failed install revokes what was
    signed; a crash between introduction and task is handled by the sweep of abandoned pending servers.
15. **The new server's instance is slot 1 locally, node id n in the catalog.** The instance directory, socket and
    marker of a server's instance are always `instances/1` (the code of the earlier increments assumes one local
    instance); the node id that `kingres.add_node` assigns differs per server (the instance id of the API is the node
    id).
16. **Node rules come first in `pg_hba.conf`.** A `cert` rule applies to TLS connections only, and on a cluster
    on one machine every connection between instances arrives from `127.0.0.1` whatever address it was made to,
    so the rule for a loopback peer also covers the loopback source addresses. They precede the fixed rules of the
    service role; a connection that presents no certificate over TLS from such an address fails in that rule.
17. **Followers do not run tasks.** The task engine now polls only while this server is the manager leader (a
    follower's `kingres_system` is an empty database made for its catalog copy). `listTasks`, `getTask` and the
    other leader-bound reads were already answered by the leader.
18. **Hidden task kind `replicate.apply`.** Role and grant changes are synchronous in the contract, so they cannot be
    tasks. They are replayed on every other server at once; a server that cannot be reached gets a task of kind
    `replicate.apply` (one step per such server, waits without limit, reads the role's *current* state when it runs
    so it never applies a stale one, stores names only, never verifiers). That kind is not in the contract's
    `TaskKind` enum, so it is not listed by the task API (`listTasks` and `getTask` skip it); it is visible in the
    log and in `kingres_cp.task`. Contract 0.4.0 should add the kind.
19. **Roles are copied as state, not as the operation.** After a role operation succeeded on the leader as the
    signed-in role, the other servers get the role's attributes, SCRAM verifier and memberships (replacing the
    memberships it has there). Dropping a role drops it everywhere. Databases and extensions: `kingres_shard` is
    installed by `kingres.add_node` on the new server (it fails when another extension made the schema `kingres`
    first), the other extensions after the registration. The system database exists on every server as the home
    of its catalog copy and nothing else.
20. **Setup state of the new server.** A join is a setup task `cluster.join` on the new server (steps: key and
    request, ask the cluster, wait for the leader), visible in `setupTask` of `GET /setup/state`; the leader's
    `server.add` is visible in `GET /tasks/{id}` after sign-in. `ResetMember` returns a removed or discarded server
    to `uninitialized` with a new setup token; it deletes the instance, key and certificates, keeps the audit file.
21. **Remote reads.** The store connects to the instances of other servers (replication topology) with the server's
    certificate as the `kingresd` role instead of the local service password.

## Ledger, alerts, metrics, this server (second part of the increment)

**Ledger on kingres_ledger 0.2.** kingresd uses the service role (a superuser) for every ledger function and for reading
the records: the functions are not executable and `ledger_digest`, `ledger_verification` and the status view are not
readable by PUBLIC, and granting them to the panel's roles would let those roles write or read the chain. Nothing is
granted at database creation; the cluster-role check (`kingres_ops` for digests and verifications) decides who asks.
Detection is by object: a database whose extension is still 0.1 has no `ledger_digest`/`ledger_verification`, so
generating a digest and starting a verification answer 422 `unsupported_operation` (naming `ALTER EXTENSION ... UPDATE`),
the digest list is empty and the verification fields of the status stay null; after the upgrade the same database
works (tested). `ledger_status` supplies the table counts and the last verification. History tables of updatable
tables are not listed as tables; the table shows `ledger.historyTable`. A verification runs `ledger_verify_recorded`
as a task (scope `full` or since the last clean one; verification against a given digest is not supported yet).
The records of verifications, the proofs and the link of a record to its task are in "Inclusion proofs and verification
records" below.
The task subject of a verification is the database name, because the verification id exists only after the run.

**Alerts** (kinds of the contract): `serverDown` (the instance is not running; since nothing can be stored while it is
down, the outage is recorded as a resolved alert when the instance is back), `diskSpaceLow` (90% warning, 95% critical),
`memoryPressure` (under 10% available), `replicationLagHigh` (over 60 s). Every 10 s; an alert is raised once per kind
and server, cleared when the reading is fine again, and a value that could not be measured neither raises nor clears.
The alert kinds `ledgerPendingTooOld`, `versionMismatch`, `certificateExpiring` and `certificateRenewalFailed` came
with contract 0.5.0 (see "Alerts raised since 0.5.0"). Not covered because the contract has no alert kind for it:
`version()` not containing "KingresDB" (logged as an error). Listing needs `kingres_readonly`,
acknowledging `kingres_ops`.

**Metrics**: a sampler (every 10 s) fills a bounded in-memory ring (1440 points per series, 2000 series): connections,
CPU, memory and disk of the server, replication lag, and per database size and transactions per second.
`GET /metrics/query` resamples it (average per step, at most 5000 points, 422 beyond). `queriesPerSecond` and
`bucketsUnderReplicated` have no samples yet (empty series). `GET /metrics` (Prometheus text) **requires
authentication**: a bearer token of a session with `kingres_stats` or better, or the scrape token `metrics-token`
(`KINGRES_METRICS_TOKEN`); without either it answers 401. It is outside the contract.

**This server**: info, resources (Linux `/proc` and `statfs`; elsewhere every value except the CPU count is null) and the
instance log (the directory comes from the configuration; entries are parsed from at most the last 1 MiB of at most 8
`postgresql-*.log` files; no path is taken from a request). Only `restartThisServerInstance` is offered: start and stop
would stop the instance that holds the control plane's own state (the task row, the sign-in), so they stay "not supported
yet". A task step that writes to the instance it restarts retries for 30 s.

**Not done in this part:** `updateClusterSettings`, server groups (create, delete, assign): the groups live in the shard
catalog (`kingres.node.server_group`) and changing them needs a catalog function this version does not have; the
local settings; `leaveCluster`.

## Third increment (several servers): what exists so far

Specification: `kingres/docs/design/control-plane-3.md`. Done: the certificate authority (D1), the internal API
(D2), the cluster settings update (part of D7), the manager leader and forwarding to it (D6), and the fixes of the
mismatches the web panel's end-to-end run found (see "Corrections after the panel's end-to-end run"), and, in this
part, joining and adding a server (D4), removing an empty server (D5), the certificate trust between the PostgreSQL
instances (D3), the cluster-wide create and drop of databases, role and grant changes and network rules, and the
integration tests with three `kingresd` processes (package `clustertest`); see "Running several servers" and design
notes 11 to 21. Tested against `kingres_shard` and `kingres_ledger` 0.2 built from the repository commit recorded by `scripts/install-extensions.sh`.
**Not done at that time** (done later, see the fourth and fifth increments): server groups and the cluster-wide
logs, alerts and metrics, the certificate renewal job, contract 0.4.0 with what depends on it (inclusion proofs,
verifications list and detail, instance start/stop, leave-cluster, the new alert kinds). Rebalance, failover and
restoring preferred primaries still answer "not supported yet".

**CA and server certificates (`internal/pki`).** `init-cluster` creates the CA in `<data-dir>/pki/` (ECDSA P-256, ten
years, `ca.key` mode 0600, written with an exclusive create) and this server's certificate (one year). The server key
is generated by the server itself; the CA only sees a certificate request. `Sign` refuses a request with a bad
signature, a key other than ECDSA P-256, another common name than the one the caller vouches for, a name outside the
allowed list, or an e-mail/URI name, and it ignores every extension of the request (the certificate is never a CA and
always carries the server and client key usages). Revoked serials are kept in `revoked.json` and published as a CRL
(`CRL`, the file PostgreSQL reads as `ssl_crl_file`). `NeedsRenewal` is true with less than a third of the lifetime
left; the renewal job is not written yet. The CA is not rotated (a limit).

**Internal API (`internal/internalapi`, specified in `internal-api.md`).** Setting `internal-listen` (env
`KINGRES_INTERNAL_LISTEN`, default empty = off) starts a separate TLS 1.3 listener with mutual TLS. **Deviation from
D2:** the spec says "the same listener"; a separate port keeps the public listener free to stay plain HTTP or to use the
operator's own certificate. A client certificate must be signed by the cluster CA, valid and not revoked; the version
header must match (the error names both versions); the call must be in the authorization table (any peer, or leader
only). The statement-execution call is leader-only and is logged with its length and hash. The server certificate
names are `server-host`, `localhost` and `127.0.0.1`. The leader is read from the catalog (below).

**The manager leader and forwarding (D6).** The leader is the server whose node holds the active primary of catalog
group 0 (`Store.ManagerLeader`, cached for two seconds by `internal/leader`; an error or an unknown leader is never
answered from a stale value, because leader-only internal calls are authorized with it). `ClusterOverview.managerLeader`,
`Server.isManagerLeader` and `LocalServerInfo.isManagerLeader` all derive from that one answer (a test asserts they
agree). A server that is not the leader forwards a mutating cluster operation (and the reads of data that only the
leader has: `listTasks`, `getTask`, `listAlerts`, `getClusterSettings`, `listLedgerVerifications`, `getLedgerVerification`) over `POST /internal/v1/forward` after it has
authenticated the caller, checked CSRF and the required role itself; the leader re-checks the operation, the identity
and the role and writes the audit event, once (`forwardedBy`, `originRequestId`). Not forwarded: sign-in/out, setup,
operations on this server, the query workbench, `createInclusionProof`. If the leader is unknown or unreachable the answer is 503 and nothing
runs locally. The internal address of every server is kept in `<data-dir>/peers.json` (`internal/peers`), because the
shard catalog knows only hosts and PostgreSQL ports; each server records itself at start when `internal-listen` is set,
and the join task (not written yet) distributes the file. The leader is also the only server that runs the task engine
and holds `kingres_cp`; a follower's copy of `kingres_system` (created when a server joins) is not used for state.
Specification of the call: `internal-api.md`.

**Cluster settings update (`updateClusterSettings`).** The values are stored in `kingres_cp.cluster_setting` and win over
the defaults the read side derives from the catalogs; a feature flag can only be turned on (the extension is then
installed in the system database).

### Corrections after the panel's end-to-end run

The panel's engineer found these mismatches against the contract; each has a regression test.

1. `GET /ledger/status` and an unfiltered `GET /grants` answered 404: they visited the system database
   `kingres_system`, which `GET /databases` does not list, so the existence check failed. **Decision:** the system
   database is hidden everywhere (the list, the count, the ledger list, the grant list, the shard-holdings scan);
   `postgres` and every other connectable non-template database is listed and counted. One function
   (`databaseNames`) defines the set.
2. `ClusterSummary.databaseCount` counted the system database; it now counts the same set as the list.
3. A table with a ledger that is neither distributed nor a reference table has `classes: ["ledger"]` (not
   `["plain","ledger"]`): `plain` is never combined with another class. The contract's wording only excluded
   `distributed` and `reference`; contract 0.4.0 must say it (not yet changed, see below).
4. The manager leader is derived once from the catalog (above).
5. A server that owns no buckets is `pendingRebalance` only while buckets are held by some server; a sharded
   database with no distributed table has none, so the server is `live` and the cluster `healthy`.
6. The table of implemented operations is checked by `TestReadmeTableMatchesTheRegistry` against the handler
   registry (and the count sentence), so it cannot lag the binary; the table was regenerated from it. Note that
   `getThisServer` and `getThisServerResources` were wrongly in the "not yet" column.

## Verification of this increment

The end-to-end and instance tests run inside WSL with `scripts/run-integration.ps1` against the private
installation in `~/kw-cp/install` (set `KINGRES_PG_BIN` to run them elsewhere); without `KINGRES_PG_BIN` they are
skipped and `go test ./...` runs the unit tests only. The runner also builds `kingresd` for Linux and sets
`KINGRESD_BIN`, which the multi-server tests of `internal/clustertest` need: they start three `kingresd` processes
(127.0.0.2, .3 and .4, each with its own data directory, ports and instance), form the cluster through the HTTP
APIs only (the first creates it, the second joins from its own API, the third is added from the cluster's API) and
cover: objects made before and after the servers joined; buckets of a distributed table on all three servers;
statements routed from every server to every other over certificates (`pg_stat_ssl` and the client certificate
name on the receiving side); sign-in with a role made on another server; a mutation sent to a follower, forwarded
and audited once; removal of an empty server, the refusal for servers that own buckets and for the map group's
server, the refusal of a removed server's certificate by the internal API and by the PostgreSQL instances; both
fingerprint modes and the wrong pin, setup token replay, wrong credentials; the internal API without a certificate,
with another CA's certificate, with another internal version and from a server that is not the leader; a join
interrupted (SIGKILL of the leader) at each of the seven steps; a failed join that cleans up; a cluster-wide task
that waits for a stopped server and finishes when it is back; the network rules on every server; and that no
password, verifier or private key is in a log, the audit file, a task row or another server's directory.
`scripts/smoke3.sh <kingresd> [base-port]` is the same flow driven by curl and psql (output in the increment's report). Since the fifth increment it also covers server groups, an inclusion proof (checked by `kingres-verify` when its path
is the third argument), stop and start of a follower's instance, a follower that leaves and is added again,
cluster-wide logs, alerts and metrics with a server down, certificates that live 30 seconds being renewed while
statements keep routing, and the revocation of a server's certificates (it prints `ok:` and `FAILED:` lines and exits
with the number of failed checks).
The fifth increment's tests are listed with their sections ("Certificates: renewal and revocation", "Cluster-wide
logs, alerts and metrics", "Alerts raised since 0.5.0", "Listings with a hard cap").
`scripts/install-extensions.sh [commit]` builds `kingres_shard` and `kingres_ledger` (both 0.2, with the fixes of their reviews) from a commit (default: the repository HEAD) into `~/kw-cp/install`, not from the working tree, and records the commit in `~/kw-cp/install/extensions-commit.txt`.

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

## Inclusion proofs and verification records

`createInclusionProof`, `listLedgerVerifications` and `getLedgerVerification` (kingres_ledger 0.2; the proof also
works with 0.1 for insert entries). Code: `internal/ops/ledger_proof.go`, `internal/ops/ledger_verifications.go`,
`internal/httpapi/handlers_ledger_proof.go`; migration 3 of the system database.

**The proof.** The 200 body is the extension's `ledger_inclusion_proof` document with its own snake_case members plus
`shard_group_id`, nothing renamed or wrapped, so the bytes the client receives can be saved and given to
`kingres-verify proof --proof` unchanged. The row is found like this: the server connects **as the signed-in
PostgreSQL role** (table privileges and row security apply: no privilege on the table is 403 `forbidden`, a row hidden
by a policy is 404) and reads `kingres.ledger_row_hash(t.*)` (or `ledger_history_row_hash` for a removed version) from
the table, matching the primary key with `jsonb_populate_record(NULL::table, <the primaryKey as one bind parameter>)`
so PostgreSQL converts every value to its column type and no value is ever part of the statement text. The proof
itself is made on a **service connection** (`ledger_inclusion_proof` is executable only by granted roles, and
granting it would let a role learn the row hashes of the whole table); the cluster role (`kingres_readonly`) and the
row read are what authorize it. It is not forwarded to the leader (it is a POST that changes nothing and runs as the
caller, like the query workbench). The `ledgerTxId` form reads the history table, which only the owner of the ledger
table can read (403 for everyone else).

| Situation | Answer |
|---|---|
| unknown database, schema, table, row, or removed version | 404 `not_found` |
| the extension is not installed in the database | 409 `conflict` (as the digest operations) |
| the row is not in a block yet (`55000`), or the ledger has no block | 409 `ledger_entry_pending` (generate a digest and ask again) |
| the table is not a ledger table; has no primary key; `ledgerTxId` or `op: D` on an append-only table | 422 `unprocessable` |
| `ledgerTxId` or `op: D` while kingres_ledger is 0.1 | 422 `unsupported_operation` |
| `op: D` without `ledgerTxId`; a primary key column missing or extra; a value that does not fit its column; bad `op`; `null`, array or object values | 400 `validation_failed` (field `/primaryKey/<column>`) |
| a body member the contract does not know | 400 `bad_request` (the decoder rejects unknown members) |
| distributed table whose distribution column is not in `primaryKey` | 400 `validation_failed` |

Limits of this increment: the lookup reads the chain of **this server's shard group** (`shard_group_id` is that
group; the other operations of the ledger do the same). A distributed table whose row is on another group has no
entry in the local chain, which answers 422 `unsupported_operation` with the reason; reading the chain of another
group needs the agent's connections and is not done. The verification of a proof against a digest is the job of the
verifier, not of the server.

**Digests.** `LedgerDigest.chains[].document` is the `digest` jsonb of `kingres.ledger_digest` (what
`ledger_generate_digest()` returned); the list and the generate operation fill it. Save it outside the database and
give it to `kingres-verify --digest`.

**Verification records.** The records are the extension's (`kingres.ledger_verification` of the user database, read on
a service connection). The control plane keeps one table, `kingres_cp.ledger_verification` (migration 3): its own
`id`, the `database_name`, the extension's `chain_id` and `verification_id`, the shard group, and the `task_id`. The
ids are allocated **the first time a record is seen**, in the order of the extension's ids (under an advisory lock, in
one statement), so they grow with time and stay unique when a database is dropped and created again (`chain_id` is
part of the key, so a stale pairing is never applied to a new chain; its extension ids start at 1 again). The task
`ledger.verify` calls `ledger_verify_recorded` and, when it returns, stores the extension's id with its own task id
(after giving the older records their ids); a run recorded without a task (SQL, the ledger worker) gets an id when it
is first listed and `taskId: null`. Because that allocation writes to the system database, both read operations are
answered by the **manager leader** (`leaderBoundReads` in `forward.go`). Mapping: `clean` is `passed`; `problems` is
`failed` with the (at most 100) problems as `inconsistencies`; the extension's `failed` is `failed` with no
inconsistencies and null counts (the cause is in the task's error). `scope` is `full` when the run started at height
0. Kinds: `block_hash` blockHashMismatch, `block_prev_hash` prevHashMismatch, `block_tx_root` and `tx_root`
merkleRootMismatch, `table_rows` and `history_rows` rowHashMismatch (field `table`), `block_height` and `tx_block`
missingBlock, every other check `other` (the detail starts with the check name). `blockHeight` is read from the
extension's object text (`block 7`). `startedBy` is the creator of the task, `system` for a run of the service role,
the extension's `started_by` otherwise. Paging is a keyset on the control plane's id; the cursor is `v<id>` (base64url),
so a cursor of another list is a 400, and a run that starts while a client pages appears only on a fresh first page.
A database with ledger mode off, without the extension or with kingres_ledger 0.1 answers 200 with `items: []`.
Not done: records of other shard groups (one chain per group needs their connections), verification against a given
digest, `state: running` (the extension never writes it).

**Tests.** `cmd/kingresd/ledger_proof_e2e_test.go` (WSL, real PostgreSQL): `TestInclusionProofsAndTheVerifier` builds
the proofs, checks each against the contract and feeds the API's bytes and the digest's `document` to `kingres-verify`
(exit 0 and `VERIFIED`; a flipped hash and a proof for another row's hash exit 1); `TestInclusionProofOnLedger01`,
`TestInclusionProofOnADatabaseWithoutLedger`, `TestLedgerVerificationRecords` (task linkage, paging, `taskId: null`,
cursors, access). The verifier binary comes from `KINGRES_VERIFY_BIN`: `scripts/run-integration.ps1` builds it
(`kingres/verifier`, standard library only) and `scripts/run-wsl-tests.sh` exports it; a proof test **fails** when
PostgreSQL is available and the variable is not set. Unit tests: `internal/ops/ledger_proof_test.go`,
`internal/httpapi/ledger_proof_test.go`.

## Server groups, updateServer and the certificate properties of Server

Operations: `createServerGroup`, `deleteServerGroup`, `updateServer` (role `kingres_ops`, all three audited and
forwarded to the manager leader); the properties `certificateExpiresAt`, `seenFingerprint`, `fingerprintPinned` of
`Server`.

1. **Where the groups live.** In the system database of the leader: `kingres_cp.server_group` (name, description) and
   `kingres_cp.server.server_group` (the assignment; foreign key `ON DELETE RESTRICT`). Both tables came with
   migration 1, so there is no new migration. Names are compared exactly (the column is `text`): `rack-a` and
   `Rack-A` are two groups. The shard catalog's column `kingres.node.server_group` is **never written** (the
   extension has no function for it); the system database is the source of truth. Where the system database has no
   `kingres_cp` schema (a bare catalog, the read-side fixtures) the store still shows the catalog's value; a recorded
   value (also "no group") replaces it.
2. **What a follower shows.** `listServerGroups` is answered by the leader (a leader-bound read, like `listTasks`).
   `listServers` and `getServer` on a follower are answered locally and then completed from the leader: the follower
   asks the leader for its `listServers` over the forward call (internal API, with the caller's identity; the leader
   accepts that one read in addition to the leader-bound ones) and takes `serverGroup`, `seenFingerprint`,
   `fingerprintPinned` and `certificateExpiresAt` from it. If the leader cannot be asked, the answer is still given
   with the values the follower has (no group, null); a read is not refused for it, a change is (503, as every
   cluster change).
3. **createServerGroup.** The body is validated as an object: an unknown member is 400 `validation_failed` with the
   member as field, so is a name outside `^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`, a description over 200 characters or a
   wrong type. 201 with `serverIds: []`; a name that exists is 409 `already_exists` (the primary key decides, so two
   concurrent creates cannot both win).
4. **deleteServerGroup.** 404 for an unknown group (and for a name that cannot be a group name); 409
   `server_group_not_empty` while any server is assigned, in any status, the detail lists the server ids; nothing is
   deleted on a refusal; 204 otherwise. A server assigned between the check and the delete is caught by the foreign
   key and answers the same 409.
5. **updateServer.** Only `serverGroup` (string or null) and `shouldHaveShards` (boolean). Any other member is 400 (a
   property of `Server` is reported as read-only, anything else as unknown); an empty object and an empty body are
   400. 404 for a server that is not in the catalog. Order: the group must exist (422 `unprocessable`, field
   `/serverGroup`); then, **only when something would change**, 409 `operation_in_progress` while the server has an
   unfinished task of kind `server.add`, `server.remove` or `server.leave` or a record in status
   `pending`/`removing`, and 409 `rebalance_in_progress` while a task of kind `rebalance` is unfinished (no rebalance
   exists yet in this version, so that check becomes reachable with it). A value equal to the current one changes
   nothing and answers 200, also during those operations. The answer is the whole `Server`.
6. **shouldHaveShards is honoured.** The catalog has `kingres.node.should_have_shards`; the leader sets it on every
   node of the server, in every database that has a `kingres` catalog, in one transaction per database: with
   `kingres.set_node_property` where the extension has it (0.3 and later) and, for 0.2 (which has the column and
   `kingres.sync_catalog()` but not the function), with the statements the function runs (lock
   `kingres.map_version`, update, `sync_catalog()`), after checking that the local instance is the map group's
   primary (otherwise 422 `node_state_invalid`). The change reaches the other servers through the catalog sync; no
   data moves until a rebalance runs. The group assignment is made in a transaction of the system database that is
   committed only after the catalog change succeeded, so a refusal by the catalog leaves the group as it was. A
   failure in the second of several databases leaves the first changed (the catalogs are separate); repeating the
   request completes it, because only nodes whose value differs are touched.
7. **Server properties.** `certificateExpiresAt`: this server's own is read from `<data-dir>/pki/server.crt`; the
   others come from the certificate the leader signed (`kingres_cp.server.certificate_pem`), so every server shows
   all of them (the creator's row has no copy, its file is used); null when neither exists. `seenFingerprint` is
   `kingres_cp.server.seen_fingerprint` (64 lower-case hex digits; anything stored that is not one is shown as null)
   and `fingerprintPinned` the recorded pin flag, false whenever the fingerprint is null. They are set only by
   `addServer` (the leader contacts the new server and records the certificate it presented). **A server that
   joined by its own request (`joinCluster`) has null and false**: it never presented a certificate to the leader;
   the fingerprint that the joining server checked (the existing server's) is in its own setup task. The first
   server has null and false.
8. **setupTask.** `NodeStateInfo.setupTask` was already served: null until a setup request was accepted on that
   server, afterwards the summary of its task (`done`, or `failed` with the error), also after a restart. Verified:
   the creator and a server that joined show `done` with 100 percent
   (`TestServerCertificateAndFingerprintProperties`), a failed join shows `failed`
   (`TestFingerprintModesAndSetupTokenReplay`), a server that the leader added shows null. Nothing had to change.
9. **Audit.** The three actions are in the `AuditAction` enum and audited by the guard; the details carry `name`
   (groups) or the changed `serverGroup`/`shouldHaveShards` (updateServer), merged with `forwardedBy` and
   `originRequestId` of a forwarded request. A handler must add to `c.details`, not replace it: replacing it drops
   `forwardedBy` (`addDetails` in `handlers_servergroups.go` does it for these three).

Tests: `internal/ops/servergroups_test.go` (validation), `internal/ops/servergroups_integration_test.go` (the store
with its refusals), `internal/httpapi/servergroups_test.go` (400/403/404 paths, the follower's completion, the
forwarded read, the local certificate), `internal/store/pgstore/servergroups_test.go` (the overlay),
`cmd/kingresd/servergroups_e2e_test.go` (one server, contract validation, audit) and
`internal/clustertest/servergroups_test.go` (three servers: create on the leader and through a follower, assign
through a follower, see it from all, refusals relayed, delete, audit, certificates and fingerprints).

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

**Revision 0.3.0 resolved items 1 to 5 below** (nullable `Server.version`, `Database.createdAt/By`, `ClusterOverview.clusterId/name`,
`LedgerStatus.mode/digestIntervalSeconds`; the meaning of `notInUniqueKey` with `TableColumn.blockingKeys`; the permitted
setup addresses; `TableColumn.type`). `Database.createdAt/By`, ledger mode and digest interval, bucket count and
durability now come from `kingres_cp.database_setting` for databases created through the API and are null for others.
The lists above that say "always null in this version" are otherwise unchanged except `ClusterOverview.managerLeader`,
which is now filled from the catalog (it is null only while the map group has no primary).

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

**Contract changes wanted for 0.4.0 by the third increment** (nothing under `kingres/api/` was changed):

8. `JoinClusterRequest` and `AddServerRequest` need an optional `expectedFingerprint` (SHA-256 of the other server's
   certificate on first contact; now the header `X-Kingres-Pin-Sha256`), and the seen fingerprint has to be returned
   (`seenFingerprint` and whether it was pinned) in the accepted task, `NodeStateInfo.setupTask` and `Server`; today it
   is only in the log, in the task's internal detail and in `kingres_cp.server`.
9. The internal API of a server has no address in the contract: it is found at the REST port plus one. Either say so
   in `NodeAddress` or let the requests carry `internalAddress`.
10. `Task` needs a free-text `detail` (and `TaskStep.detail`): a task that waits for a server says which one in
    `progress.currentStep` ("Create the database on server X") today, which works but is a convention.
11. `TaskKind` needs the housekeeping kind `replicate.apply` (hidden now), and the `subject.type` of `server.add` and
    `server.remove` is `server` with the server id.
12. `removeServer` is described as draining through a rebalance; this version removes only a server that owns no
    buckets and answers 409 otherwise (the draining removal needs rebalance). The description and the conflict text
    should say which. `addServer` and `joinCluster` should say that the server stays `live` (not `pendingRebalance`) while
    no database has buckets.
13. A way to revoke a server certificate without removing the server (`revokeServerCertificates`), to renew
    one now (`renewServerCertificates`) and the dedicated problem for a leader without the CA key came with contract
    0.5.0.

## Starting and stopping the instance

`startThisServerInstance` and `stopThisServerInstance` (`kingres_ops`) and `restartThisServerInstance` act on the
PostgreSQL instance of **this** server only; they are never forwarded to the leader (tag "This server"). Code:
`internal/ops/instancectl.go` (states, conflicts, the gate), `internal/ops/instancerun.go` (the three runs, the task
records), `internal/ops/taskrecorder.go` (who writes the tasks), `internal/httpapi/handlers_instance.go` (handlers,
the 503 gate, the start from memory). The instance id of the path is the catalog's `node_id` of this server's instance
(read from the catalog while it can be read and remembered; `1` when nothing is known); another id is 404. The body
`{"force": bool}` of stop and restart is optional; anything else in it is 400.

**States and conflicts.** The state is the in-memory operation (`starting`, `stopping`, `restarting`) or else follows
the process (`pg_ctl status`): `running` or `stopped`. Stop: `stopped`/`stopping` is 409 `instance_already_stopped`,
`starting`/`restarting` is 409 `operation_in_progress`. Start: `running`/`starting`/`restarting` is 409
`instance_already_running`, `stopping` is 409 `operation_in_progress`. Restart: like stop. An unfinished task of the
instance in the task table is also `operation_in_progress`. `no_replica_available` (stop and restart, not start) means
the instance is the only up-to-date copy of its shard group while replicas exist that are not caught up (a standby is
"up to date" when it runs and is at most 1 MiB behind; an unknown lag counts as behind; a group without replicas never
blocks); `force: true` overrides this and only this.

**Tasks.** `instance.start`, `instance.stop` and `instance.restart` have subject `instance/<node id>` and one step
("Start the instance", "Stop the instance", "Restart the instance"). They are **not run by the task engine** (the
engine writes a step only after it ran, and the database that holds the task is hosted by the instance the task
starts or stops, or, on a follower, does not have the task table at all): the owner writes them through a
`TaskRecorder`. On the leader that is its own task engine (`task.RecordStore`, new in `internal/task/record.go`:
`Record`, `Settle`, `ReserveID`); on any other server it is the leader, through three new internal calls (see
`internal-api.md`: `POST /task/record`, `/task/settle`, `/task/reserve`, answered only by the leader, limited to the
three kinds with an instance subject). The flows:

* **stop**: the task is recorded `running` and returned with 202; just before the shutdown it is settled `done` (all
  steps done, 100 percent), then `pg_ctl stop -m fast`; if the instance does not stop (or still runs) the task is
  corrected to `failed` (error `step_failed`, retryable) and everything is as before. While the database is still up
  an id is reserved from the task sequence for the start that follows.
* **start**: the system database of the leader is down, so nothing can be written: the task is announced with the
  reserved id (or, when none exists, with a number of microseconds since 1970, far above any sequence value and
  below 2^53), kept in memory, and written finished (`done` or `failed`) under that id once the instance runs and a
  service connection to the system database works; `getTask` answers 503 `system_database_stopped` until then. A
  follower's start is recorded with the leader at once (the leader is up) and settled when the instance is back.
  When a write cannot be made the finished task stays in memory and is written at the next successful start.
* **restart**: recorded and settled the same way (settled when the system database is back; the earlier engine step
  "retries 30 seconds" is replaced by waiting for the database). Restart used the engine before, which made it fail
  with 500 on a follower (no task table there); it now works on every server.

If kingresd is killed between the record and the settle, the row keeps a lease of 15 minutes
(`task.RecordLease`); then the leader's engine claims it and the definitions in `InstanceDefinitions` fail it ("the
server ended before the operation finished"). Until then the instance has an unfinished task (409
`operation_in_progress` for a new operation).

**While the instance is down (stopped on purpose).** From just before the shutdown until the start has finished,
`Ops.LocalInstanceDown()` is true and the guard (`gateStoppedInstance`, before authentication) answers **503
`system_database_stopped`** (with `Retry-After: 10`, naming `POST .../this-server/instances/<id>/start` in the detail)
for `login` and for every operation with a session, except `startThisServerInstance`. `getHealth`, `getNodeState` and
the setup operations are not gated and answer as always. **`getReadiness` answers 200** with the check
`localPostgres` `ok: false` and a detail while the instance is stopped on purpose (the process is ready to serve the
start; a load balancer that needs the database must look at the check); it still answers 503 when the instance is down
for any other reason. The stopped state ends when the start finishes, and also when an operator starts the instance on
the machine (probed at most every 2 seconds). A restart of kingresd starts the instance again (`Resume`): the stopped
state is not persisted, so a deliberate stop does not survive a reboot or a service restart of kingresd.

**The start from memory (CP-02).** The revalidation reads `pg_roles` of this server's instance and fails closed with
503 when it cannot (that is unchanged for every operation). For `startThisServerInstance` only, when the fresh check
fails with 503 **and** the instance is verifiably not running (stopped on purpose, or not running for any reason; never
while it runs), the session is authorized with the cluster roles of the last successful check of that role
(`auth.Revalidator.LastGood`, kept past the 3-second cache, forgotten by `Changed` and by any refusal) or, when the
process has not checked that role since it started, the roles of the sign-in, if that check is not older than
`Deps.StaleRolesMaxAge` (default one hour). The session itself (in memory) must still be valid: an expired or revoked
token is 401. A check that is too old answers 503 (`system_database_stopped` while the instance is down on purpose).
Tests: `internal/httpapi/instance_test.go` (`TestStartFromMemoryDoesNotWeakenRevalidation`: a running instance still
fails closed also for the start; the other operations stay 503 while it is stopped; a role that lost its cluster roles
is not revived; a check older than the bound is refused; an expired session is 401) and
`internal/auth/lastgood_test.go`.

**A server that forwards.** A follower that forwards a mutating operation or a leader-bound read (`listTasks`,
`getTask`, `listAlerts`, `getClusterSettings`) to a leader whose instance is stopped relays the leader's 503
`system_database_stopped` (the leader's guard answers it). The leader's internal API cannot read the catalog while its
instance is down, so it authorizes with the leader it knew last (`cmd/kingresd/leadermemory.go`); there is no failover
and the leader still checks that it is the leader, so this adds no power to a peer. The same remembered leader is what
a stopped follower uses to find the leader that records its start task.

**What else happens while the instance is down.** There is one instance per server in this version, so "other
instances" are the instances of the other servers: they keep running and keep serving; the catalog is not updated by
the stop (the leader keeps listing a stopped follower as `live`; there is no heartbeat, a server is `dead` only when
the catalog says so), cluster changes made on the leader meanwhile reach the stopped server later through the
`replicate.apply` task that every role or grant change starts for a server it could not reach (observed in
`TestFollowerInstanceStopAndStart`). Followers keep answering what they read from their own instance (observed:
`getThisServer`, `getCluster` 200); what is the leader's answers the leader's 503. The monitor's sampler keeps
sampling the operating-system metrics of the server (the instance-dependent ones are skipped), remembers the outage
(`downSince`) and, when the instance runs again, records it and raises the alert `serverDown` (so the alert appears
when the server is back, as the contract wants, within one sampler interval of 10 seconds); a stop shorter than one
interval is not seen by it. No ledger digest, verification or other background work of this process needs the
instance except through the task engine and the sampler, so nothing else runs in the meantime; the engine of the
leader keeps polling and fails quietly until the database is back. The panel, the audit file and the logs are not
affected (the audit log is a file).

**Not done.** The stopped state of a deliberate stop is not persisted across a restart of kingresd (see above). The
leader's `listServers` does not show a stopped follower as down. `start` while the leader is also stopped keeps the
follower's task in memory only (it is written at the next start).

## Local settings of this server

`getThisServerSettings` (`kingres_readonly`) and `updateThisServerSettings` (`kingres_admin`), in
`internal/localsettings`. They are per server and are never forwarded; each server answers from its own files, so they
also work on a standalone server and while the leader is down.

* **Where the values are.** The memory quota and the log directory are in the instance's managed settings
  (`instance.ManagedSettings.MemoryMegabytes` and `LogDirectory`, recorded in the instance's `settings.json` and
  rendered into `kingres-managed.conf`; the setup and the join now record the quota there, the old `Extra` values
  `shared_buffers`, `effective_cache_size`, `maintenance_work_mem` remain valid and are replaced when the quota is
  changed). `shared_buffers` is a quarter of the quota, `effective_cache_size` half, `maintenance_work_mem` a sixteenth
  up to 2 GiB (`instance.MemorySettings`); `log_directory` is `log` (below the data directory) unless set. An instance
  without the recorded quota reports `shared_buffers * 4`, else 1024. The log level of kingresd and the "restart
  pending" mark are in `<data-dir>/local-settings.json`. A saved log level is applied at start and wins over the
  configured `log-level` until it is changed again.
* **Read.** `dataPaths.dataDirectory` is the data directory of kingresd (`walDirectory` is absent, `logDirectory` is
  present when set), `clientPorts.readWritePort` is the PostgreSQL port of this server (there is no router and no
  read-only port: `readOnlyPort` is `null`), `logLevel` is `debug`, `info`, `warning` or `error`.
* **Update (400 `validation_failed`, `errors[].field` is a JSON pointer).** `dataPaths` and `clientPorts` ("cannot be
  changed after the server was created"), any other member, an empty object, a wrong type, a log level outside the
  enumeration, `memoryQuota.totalMegabytes` outside 512 to 16777216 (or missing, or an unknown member of
  `memoryQuota`), and a `logDirectory` that is an empty string. **422 `unprocessable`**: a quota larger than the
  physical memory when it is known (`/memoryQuota/totalMegabytes`), a `logDirectory` that is not an absolute clean path
  starting with `/`, does not exist, is not a directory or cannot be written by kingresd (`/logDirectory`). One refused
  member refuses the whole request; nothing changes. The audit entry records the operation id (and in `details`
  whether a restart is now required); the settings are never written to a log.
* **Applying.** `logLevel` is applied at once (`slog.LevelVar`) and never needs a restart. A changed quota or log
  directory is written to the managed configuration and applies **when the instance is restarted** (kingresd does not
  reload it, so an instance never runs half of a change; the contract's "restartRequired: true for both" is therefore
  also what PostgreSQL's contexts need: `shared_buffers` is `postmaster`, `log_directory` is `sighup`). A value equal to
  the current one changes nothing. A change while the instance is stopped is read by the next start.
* **`restartRequired` is persistent.** It is true from the change until the postmaster that was running at that moment
  has been replaced: the state file keeps `pid:start time` of `postmaster.pid` (`instance.Manager.PostmasterToken`),
  and `restartRequired` is "the token of the running postmaster equals the saved one". Every way of restarting
  (`restartThisServerInstance`, stop and start, an operator, a reboot) ends it, kingresd's own restart does not, and no
  code path that restarts the instance needs a hook.

## Fix round for the second review (contract 0.6.0)

Source: `kingres/docs/reviews/2026-11-15-kingresd-0.4.md` (CP2-01 to CP2-22, the unconfirmed list) and the defects the
other engineers reported afterwards. Every finding below has a test that fails without the fix; where "before" could
not be run (the old API cannot express the test) it is said. This section supersedes older statements of this file
where they disagree (for example "Not done: records of other shard groups" and the "few seconds" bound of role
revocation). Contract 0.6.0 and its breaking and additive changes: `kingres/api/README.md`, section "0.6.0".

### Findings and what changed

| Finding | Result | What changed | Test |
|---|---|---|---|
| CP2-01 / CP-07 forwarded identity by name | fixed | Per-request SCRAM-style proof of a session of the named user, checked against the user's verifier by the leader only (`internal/fwdproof`, `internal/scram`, `auth/proofkey.go`); cluster-wide reads carry an assertion signed by the leader (`POST /gather-authorize`) instead of a proof, so no other member ever receives a value from which a ClientKey can be derived; pending servers authorize nothing; `Proxy.Allow` runs after the guard. See `internal-api.md`, "Forwarded identities need a proof". | `fwdproof` tests, `internalapi` `TestPendingServerAuthorizesNothing`, `TestUnprovenIdentityIsRefusedBeforeTheBackend`, clustertest `TestForwardedIdentityNeedsTheProofOfASession` (real three-server cluster; with the check switched off the same forward without a proof answered 200) |
| CP2-02 ops to admin through `addServer` | fixed (breaking) | `addServer` needs `kingres_admin`; a certificate is issued only for a request an administrator made and that names the candidate's certificate; verifiers only after that; a pending server authorizes nothing | clustertest `TestOpsCannotAddAServer`. The run before the fix was not completed, so the first half (the leader signs for an ops caller's server) is confirmed by reading only |
| CP2-03 pin dropped through a follower | fixed | the receiving server normalizes body and header pin (malformed: 400 everywhere) and forwards one value; the leader uses it for both sources | `httpapi` `TestAddServerPinThroughAFollower` (13 cases) |
| CP2-05 password before fingerprint | fixed (breaking) | `joinCluster` and `addServer` need a pin or an explicit `allowUnpinned: true` | `TestAddServerPinThroughAFollower`, clustertest `TestJoinPinFlowFromThePanels` |
| CP2-04 renewal and CRL refresh | verified closed, not redone | `membership/certjob.go` renews and refreshes the CRL | `TestRenewalJob*`, clustertest `TestCertificateRenewalKeepsAThreeServerClusterWorking` |
| CP2-06 task leases | fixed | lease arithmetic on the database clock (`clock_timestamp()`), fencing epoch `task.lease_epoch` checked in every write, the `running` mark atomic with the lease check, the step context is cancelled when the lease cannot be kept | `task` `TestSkewedClockCannotTakeALiveLease` (two engines, 5 minutes skew), `TestStepStopsWhenTheLeaseCannotBeRenewed`, `TestFencingEpochSeparatesRunsOfTheSameHolder`, `TestNoStepStartsOnAnExpiredLease`, `TestPGStoreLeaseUsesTheDatabaseClock` |
| CP2-07 revocation on unreachable servers | partly fixed, rest deferred | wrong "few seconds" claim corrected; alert kind `replicationPending` for a server the leader still waits for. Deferred: followers refusing sessions after losing the leader (needs the heartbeat of the next phase); the response of the revoking operation does not list unapplied servers | `monitor` `TestReplicationPendingConditions`, `TestReadReplicationAgainstPostgreSQL` |
| CP2-08 digests and verification of one group | fixed (truthful scope) | `complete` and `uncoveredShardGroupIds` on `LedgerDigest` and `LedgerVerification`; a task `done` always leaves a record | `ops` `TestDigestAndVerificationSayWhatTheyCover`, `TestOtherGroupsReadsTheShardCatalog`, e2e `TestLedgerDigestScopeAttributionAndBytes` |
| CP2-09 sessions lock-out | fixed | 20 sessions per role, oldest first; global cap as backstop | `auth` `TestStorePerUserCap` |
| CP2-10 limiter lock-out | fixed | a full table evicts instead of refusing; IPv6 by /64; health probes exempt | `auth` `TestLimiterSaturationDoesNotLockNewKeysOut`, `TestAddressKeyGroupsIPv6By64`, `httpapi` `TestPublicEndpointsAreRateLimited` |
| CP2-11 cut DML | fixed | non-read statements are read to the end and committed (`truncated`, real tag); plain reads are stopped and rolled back | e2e `TestWorkbenchCutDMLIsCommitted` |
| CP2-12 `timeoutMs` | fixed | own context for `executeQuery`, write deadline `timeoutMs + 15 s`, 422 `sql_error` 57014 | e2e `TestWorkbenchTimeoutIsASQLError` |
| CP2-13 audit on a follower | fixed | `listAuditEvents` is cluster-wide through gather with `partial`/`missingServers`; event ids are cluster-unique; `Sink.Ready` refuses an audited mutation (503) before any effect when the log cannot be written | `httpapi` `TestAuditListIsClusterWideAndPaged`, `TestMutationIsRefusedWhileTheAuditLogCannotBeWritten`, clustertest `TestAuditShowsTheLeadersActionsOnEveryServer` |
| CP2-14 removal order, CA key | fixed, partly tested | revoke before the irreversible reset; a pending record is kept when it cannot be revoked; a registered pending server whose add never finished is revoked after 24 h | `membership` `TestRemovalRevokesBeforeTheIrreversibleStep` (the other two paths have no seam without a system database) |
| CP2-15 workbench slots | fixed | two of four slots per role | `TestOneRoleCannotHoldEveryWorkbenchSlot` |
| CP2-16 audit scan | fixed | backward scan to `Limit+1`; rotation at 64 MiB into `audit.jsonl.<first id>`, segments are never deleted | `audit` `TestFileSinkRotatesAndQueriesAcrossSegments`, `TestFileSinkBackwardReadHandlesBlockBoundaries` |
| CP2-17 log directory | fixed | the directory comes from the managed `log_directory` | `instance` `TestLogDirFollowsTheManagedSettings`, `httpapi` `TestLogsFollowAMovedLogDirectory` |
| CP2-18 durable revocations | fixed | `pki.WriteFile` syncs the file and the directory | `pki` `TestWriteFileFlushesBeforeAndAfterTheRename` |
| CP2-19 `LastGood` | fixed | `good` is stored only when the epoch is unchanged; the start fallback to sign-in roles is gone | `auth` `TestRevalidatorChangedIsNotUndoneByAnInFlightCheck`, `httpapi` `TestStartFromMemoryDoesNotWeakenRevalidation` |
| CP2-20 peer task calls | fixed | a peer may record and settle only tasks of its own instances | `ops` `TestHandleTaskCallBindsTheCallToTheCallersInstances` |
| CP2-21 plaintext listener | fixed as a warning, not a refusal | a WARN at every start for a non-loopback listener without TLS; a refusal needs a packaging flag, deferred. `getNodeState` and `getReadiness` stay public by contract and say nothing secret | `config` `TestPlaintextOnNetwork` |
| CP2-22 stop settled first | documented, not changed | see "Stop is settled before the stop" below | none |
| Unconfirmed: digest attribution | confirmed, fixed | `kingres_cp.ledger_digest_actor`; a digest of the service role is `system` | e2e `TestLedgerDigestScopeAttributionAndBytes` (`generatedBy` was `kingresd`) |
| Unconfirmed: CRL expiry | not re-run; CP2-04 removes the cause (the CRL is refreshed well inside its validity) | | `TestRenewalJob*` |
| Unconfirmed: one huge value | confirmed, fixed | `internal/pgframe` closes a connection whose message header announces more than 16 MiB before the body is read (286 MiB were allocated before) | `pgframe` tests, e2e `TestWorkbenchHugeValueIsBounded` |
| Unconfirmed: case-insensitive names | confirmed by reading, fixed | a server id that differs only by case from a member is refused (409) | `membership` `TestCaseCollision` (unit; not run with two real servers) |
| Unconfirmed: exec without statement timeout | confirmed, fixed | `execLocal` sets `statement_timeout` to 15 minutes | clustertest `TestInternalExecRunsUnderAStatementTimeout` |
| Unconfirmed: leader change under a pause | refuted as far as this code goes | with CP2-06 a paused old leader cannot start a step after its lease ended (the mark is atomic with the lease) nor write after another claim (epoch); the election itself is the extension's | `TestNoStepStartsOnAnExpiredLease`, `TestFencingEpochSeparatesRunsOfTheSameHolder` |
| Unconfirmed: ledger byte-exactness | refuted (sound) | the API's `document` equals the stored `digest::text` with white space removed; the verifier checks the API bytes | e2e `TestLedgerDigestScopeAttributionAndBytes`, `TestInclusionProofsAndTheVerifier` |
| Defect 1 `getDatabase` on a follower | fixed (server only) | the follower takes the recorded fields (bucket count, ledger mode, digest interval, creator) from the leader, whose system database holds them | `httpapi` `TestFollowerAnswersTheRecordedFieldsOfADatabase`, clustertest `TestDatabaseRecordedFieldsAreTheSameOnEveryServer`. `packaging/test/test-compose.sh` was not edited |
| Defect 2 `joinedAt`, `uptimeSeconds`, status | fixed | `joinedAt` was never set; `uptimeSeconds` only for the answering server (the others are asked, null when silent); `pendingRebalance` is now "should hold shards, is primary of no bucket, buckets exist" (before, it depended on how far the standby's copy of a neighbour's buckets had come) | `pgstore` `TestStatusPendingRebalanceNeedsBucketsElsewhere`, clustertest `TestJoinedServersHaveJoinedAtUptimeAndOneStatus` |
| Defect 3 sessions per server | fixed (contract text) | `login`, `sessionCookie`, `bearerAuth` | `npm run check` |
| Defect 4 503 window | fixed | `down` is set when a stop or restart is accepted; a connection lost while it is down is `system_database_stopped` | `ops` `TestStopAndRestartGateFromTheStart`, `httpapi` `TestStoreFailureWhileTheInstanceIsDownIsTheDocumentedCode` |
| Defect 5 no internal API | fixed | 409 `internal_api_disabled` (additive) | clustertest `TestServerWithoutInternalAPIAnswersADedicatedCode` |
| Defect 6 `walDirectory` | fixed | the answer always has `walDirectory` and `logDirectory`, null when default; the `required` list stays `[dataDirectory]` | `localsettings` `TestAnswerAlwaysNamesTheOptionalDirectories` |
| Defect 7 TypeScript types | fixed (contract) | `LedgerProofBlock`; `document` must be kept byte-exact | `npm run check` |
| Defect 8 `SetupTaskSummary.detail` | fixed | additive | `TestJoinPinFlowFromThePanels` |
| Defect 9 own fingerprint | fixed | `NodeStateInfo.internalFingerprint` (loopback or setup token holder only), `LocalServerInfo.internalFingerprint` (any signed-in role) | `TestJoinPinFlowFromThePanels` |
| Panel review W2-01/W2-02: no session after a reload while the system database is stopped | fixed | `getSession` and `listThisServerInstances` answer from memory in the stopped state, authorized from `Revalidator.LastGood` (at most one hour, ended by any role change, never for a session this process did not check); the answer is marked `degraded`. Residue: a dropped or disabled role keeps its own session, the instance list and the start of this server for at most one hour and only until any role change on the cluster (the CP2-07 residue, not wider). After any role change on the leader a stopped follower can only be started on the machine | `httpapi` `TestSessionAndInstancesWhileTheSystemDatabaseIsStopped`, e2e `TestInstanceStopStartEndToEnd`, clustertest `TestFollowerInstanceStopAndStart`, `TestStoppedFollowerSessionRules` |
| Wishes: `Session.clientAddress`, dry run of `replaceNetworkAccess` | done (additive); cancelling a workbench statement is deferred | | e2e `TestNetworkAccess` |
| Extra defect found by testing | fixed | the workbench's dangerous-role check counted the bare ADMIN membership of a role's creator and locked administrators out | e2e `TestWorkbenchIgnoresBareAdminMemberships` |

### Decisions

1. **Forwarded identities carry a SCRAM-style proof** checked against the user's verifier, sent to the leader only (a receiver of a proof recovers the ClientKey, as a SCRAM server does; a gather therefore carries an assertion signed by the leader, not a proof). A signed assertion of the
   forwarding server's certificate was rejected: the member's key signs anything, the proof needs a secret that only
   a server that saw the password has.
2. **A pending server authorizes nothing** except its own join: every call to the leader needs catalog membership.
3. **`addServer` needs `kingres_admin`**; a pin is mandatory for add and join unless `allowUnpinned: true`.
4. **The fingerprint of a server that is not a member yet** is shown only to the machine itself or to the holder of
   the setup token; a member's fingerprint to any signed-in role.
5. **Leases are on the database clock**; the engine's own clock only measures time since a confirmation, never an expiry.
6. **Digests and verifications state what they cover** (`complete`, `uncoveredShardGroupIds`) instead of covering every
   group now. Never `passed` for a part without saying so.
7. **The audit list is cluster-wide** (gather), not leader-bound; a mutation is refused when the audit log cannot be
   written (pre-flight, not an intent record, which would double the log and need a new enum value).
8. **Limiter tables evict, never refuse**; sessions are capped per role.
9. **`pendingRebalance` means "primary of no bucket"**, not "holds nothing".
10. **A plaintext non-loopback listener warns**, it does not refuse (packaging decides).
11. **Revocation lag is an alert** (`replicationPending`), not a bound; a bound needs the heartbeat of the next phase.
12. **`down` is set when a stop or restart is accepted**, so the documented code is the only one seen.

### What a compromised member can still do

See `internal-api.md`, "Forwarded identities need a proof". In short: a member holds its own superuser, every role's
SCRAM verifier and the proof key of every session opened on it. It can act as any user who signs in on it, run SQL on
itself and (through the sharding extension's node connections) on the data routed to it, and attack the verifiers
offline. Since 0.6.0 it can no longer act as a user who never signed in on it, and a server that is only pending can do
nothing. A compromised leader is a compromised cluster (it receives the proofs, holds the CA key and runs every
cluster operation). The rest is inherent: every server authenticates users itself and holds the verifiers, so a compromised
member is a compromised server for the users who use it.

### Stop is settled before the stop (CP2-22)

The task row of an instance stop lives in the database that is about to stop, so it can only be settled before the
shutdown; it is corrected to `failed` when the instance does not stop. A crash of `kingresd` between the two leaves a
`done` task and a running instance. This is a deliberate choice, not an oversight.

### Limits that remain

- The residual of CP2-06: a process paused between the atomic `running` mark and the first side effect of a
  non-idempotent step can still act after its lease ended. Since the sixth increment every remote call of a step
  carries the fencing epoch and the callee refuses a lower one (see "Carry-overs"); the residue is a late call to a
  callee that the newer run has not called yet, and an effect of the step on its own server.
- Digests and verifications still cover only the chain of the server that makes them.
- A cluster-wide read is refused (the server shows as missing) when the leader cannot be reached or cannot check the proof.
- The replay cache of forwarded proofs is per server and lost on restart (a replay needs a proof captured from a
  TLS channel and falls inside the two-minute window).
- The leader's `ClusterOverview` counts use each server's own catalog view.
- Events of a removed server are gone with it (audit); a disk that fills between the pre-flight check and the final
  audit record only logs the failure.
- Schema version is 5 (`0005_task_fence.sql`: `task.lease_epoch`, `ledger_digest_actor`).

## Sixth increment: high availability, part 1 (contract 0.7.0)

Status of the work in this section: see the end of each subsection. The design is `kingres/docs/design/ha-phase5.md`
(part A, part B up to the promotion step, part D). The promotion (catalog swap, `failoverServer`,
`restorePreferredPrimaries`) needs the sharding extension 0.4 and is part 2.

### Heartbeats and liveness

Every server sends every other server of the peer directory a heartbeat (`POST /internal/v1/heartbeat`, see
`internal-api.md`) each `heartbeat-interval`, and a server that answers a heartbeat has proven itself alive just as
one that sends it has. Package `internal/liveness` keeps, per server, the monotonic time (package `internal/clock`) at
which this server last heard from it. Nothing that travels carries a time: the receiver notes the arrival on its own
monotonic clock, so a step of the system clock changes nothing but the wall time shown as `lastHeartbeatAt`.

| Flag / file key | Environment variable | Default | Meaning |
|---|---|---|---|
| `heartbeat-interval` | `KINGRES_HEARTBEAT_INTERVAL` | `1s` | how often a heartbeat is sent to every other server; at most half of `suspect-timeout` |
| `suspect-timeout` | `KINGRES_SUSPECT_TIMEOUT` | `5s` | silence after which a server is shown as `suspect` |
| `failure-timeout` | `KINGRES_FAILURE_TIMEOUT` | `10s` | silence after which a server is shown as `dead` (the contract text: no liveness for the failure timeout) |
| `leader-loss-bound` | `KINGRES_LEADER_LOSS_BOUND` | `30s` | how long a server that cannot hear the manager leader keeps authorizing sessions from its own copy of roles and grants |
| `fault-dir` | `KINGRES_FAULT_DIR` | empty (off) | **tests only**: the fault-injection hook (`internal/fault`) reads `faults.json` there; see "Tests" below |

**Status.** `GET /servers`, `GET /servers/{id}` and the counts of `GET /cluster` put the heartbeats over the catalog
status: silent for `suspect-timeout` is `suspect`, silent for `failure-timeout` is `dead`, and when the server is heard
again the status the catalog gives (`live`, `pendingRebalance`, `draining`) shows again, because nothing is stored.
A server that never answered since this server learned of it becomes `dead` after the failure timeout too; before the
first suspect timeout it keeps the catalog status. `lastHeartbeatAt` is the wall time of the last heartbeat, derived
from its monotonic age (so it moves with a wall-clock step; the decision does not); for this server it is the current
time. Servers that are only pending (an add that is running) are not listed, so they cannot be shown dead. A server
whose instance was stopped on purpose still heartbeats (kingresd runs), so it is not dead.

**One view.** The counts of the overview are made from the very list `GET /servers` builds (one catalog snapshot, one
status function), so for one server the counts and the list agree. **Each server answers from its own heartbeat view**
(all servers heartbeat all others): a follower can show `suspect` for a server a moment before the leader does. The
leader view is the one the alerts follow.

**Alerts.** While a server is the manager leader it raises `serverSuspect` (warning) and `serverDown` (critical, the
existing kind) for the other servers in its own alert table, with the subject `server:<id>`, and clears them when the
server is heard again (`internal/monitor/liveness.go`, reconciled by `cmd/kingresd/ha_liveness.go` every interval on a
change and every ten intervals otherwise, so a restarted leader recovers them). The `serverDown` that a server raises
about its own stopped instance is unchanged and lives in that server table. An alert about a server that was removed
from the cluster while it was dead is not cleaned up by this code (limit).

**A stalled process does not judge the others.** A kingresd that was frozen (SIGSTOP, a suspended virtual machine) and
wakes up has not heard from anybody for longer than the failure timeout by its own count. The tracker notices a gap of
more than three intervals between its own ticks, answers meanwhile what it knew before the stall and, at the next
tick, gives every server a fresh window (`TestStalledProcessDoesNotJudgeItsPeers`, and in the cluster
`TestHeartbeatsAndLiveness/a_paused_process...`).

**The cached view and the leader (the deferred half of CP2-07).** Roles, grants and settings are changed on the
leader, which pushes the change to every server (`replicate.apply`). A follower that cannot hear the leader cannot
learn of a revoked role. Decision, and exactly which decisions need the leader:

- A server trusts its own copy for authorizing a **session** (cookie, bearer token, forwarded identity: every
  operation that goes through `currentRoles` of the guard) while it is the leader, while it has never learned a
  leader, and while the leader was heard within `leader-loss-bound`. Beyond the bound every such request answers
  `503 leader_unavailable` with `Retry-After: 5`, and the server serves again as soon as the leader is heard
  (`httpapi.Deps.LeaderTrusted`, `leaderTrusted` in `cmd/kingresd/ha_liveness.go`). The leader is heard through the
  heartbeat in either direction.
- Everything that needs no session is unchanged: the health and readiness probes, `getSession` and the start of a
  stopped instance in the stopped state (these use `LastGood`, bounded to one hour and flagged `degraded`, as before).
- A blip shorter than the bound changes nothing: `TestHeartbeatsAndLiveness/a_short_blip...` reads on an isolated
  follower for five seconds and every read answers 200. The default bound of 30 s is three times the default
  failure timeout.
- The residue that stays: for up to `leader-loss-bound` after the contact is lost a follower authorizes from its copy,
  as it did before (a role revoked on the leader during that time can still work on the isolated follower for that
  long). The leader is told by the heartbeat only; a leader that is reachable but whose kingresd is wedged is judged
  by its answers like any server.

**Tests.** Unit: `liveness` (fake clocks, a wall-clock step, a stalled process, the beater), `monitor`
(`TestPeerLivenessConditions`). Cluster (`internal/clustertest/liveness_test.go`, three real kingresd processes with
short bounds 0.5 s / 3 s / 6 s / 12 s): a killed server is `dead` on every server within the bound (measured: 5.7 s
after the kill on the leader), `serverDown` raised and cleared, the counts agree with the list, the status after the
restart equals the one before; a one-way cut (one server cannot call another, the answers still reach it) changes
nothing for 14 s; a partition shows both sides dead to each other and heals; a follower isolated from the leader refuses
sessions after the bound and not before; a paused process does not declare the others dead when it wakes; a step of
the wall clock (hook `clockOffsetMs`) changes nothing. The hook of `fault-dir` is read by the peer client (outbound
calls fail as unreachable) and the internal API (inbound calls are dropped without an answer); it exists only so that
tests can cut the network inside the process without firewall rules or root, and is off unless `fault-dir` is set.
`scripts/smoke3.sh` step 15b stops a server and checks that the others list it as dead and then as alive again.

### Leases and fencing

What exists (design `ha-phase5.md`, "Fencing by lease"; code in `internal/lease`, `cmd/kingresd/ha_leases.go`,
`cmd/kingres-watchdog`):

- **Grantor.** Every server answers `POST /internal/v1/lease/grant` (see `internal-api.md`). It keeps one holder per
  shard group and refuses every other holder until the lease has expired **plus** the safety margin. A grant that
  changes the holder is written to `<data-dir>/lease/grants.json` (temporary file, fsync, atomic rename, fsync of the
  directory) **before** it is answered; if it cannot be written it is not granted. A restarted grantor reads the file and
  assumes every grant in it was made just now for its full duration: it refuses other holders for `lease-duration` plus
  `lease-margin` after the restart, which is the longest a lease can still be alive. A grants file that cannot be read
  stops the grantor (and with it kingresd) at start: an unreadable record is not the same as an empty one.
- **Holder.** The `kingresd` that runs a group's primary (the group whose `primary` node in the catalog has this server's
  host and port) renews every `lease-renew`. A round asks every server of the catalog (itself through its own grantor,
  without the network) and counts the grants; with a majority of the catalog's distinct hosts the lease is valid until
  **the start of the round** plus `lease-duration`, on this server's monotonic clock. Time of day is never compared:
  only durations travel (the request carries `durationMs`, the refusal `waitMs`).
- **Fencing.** The holder acts on a lease only until `lease-duration` minus `lease-margin` after the round start
  (`Holds`). A check every 100 ms (`lease.Manager.Check`) fences when that time has passed without a successful round:
  it writes the **fence marker** `<data-dir>/lease/fenced`, then stops the instance with `pg_ctl stop -m immediate`
  (no checkpoint, no waiting for clients), and audits `leaseLost` and `leaseFenced`. While the marker exists the instance
  manager's start gate refuses to start the instance (`instance.ErrLeaseRequired`: the instance start operations fail,
  the start at kingresd's own start is skipped with a warning; decision 7 adds the wait marker for a cold start). The next successful round removes the marker, starts
  the instance and audits `leaseAcquired`. The marker is on disk, so a restart of kingresd or of the machine does not
  start the primary without a lease either.
- **Enforcement rule (decided).** Self-fencing is enforced only for a group that has at least one standby (the standby
  manager answers `Standbys().HasStandby(group)` from local knowledge). For a group without a standby nothing could be
  promoted, so stopping the only copy would cost availability and buy nothing: the lease is still acquired, renewed and
  reported, and its loss raises the alert `leaseLost` (critical, subject `server:<id>|group:<n>`) and the audit event
  `leaseLost`, without stopping the instance. A one-server cluster has one member and a majority of one: it grants itself
  its lease.
- **Watchdog.** `kingres-watchdog` (standard library only, about 90 lines) covers a dead or hung `kingresd` next to a
  live primary. For an enforced group kingresd starts it after the first successful round, detached in its own session
  (a signal to kingresd's process group does not reach it; it takes an exclusive lock so one runs per instance). After every
  renewal kingresd replaces `<data-dir>/instances/1/lease.json` with `{"seq": N, "validMs": V}`: `seq` changes with every
  write and `V` is how long the lease still lets the primary run (round start plus duration minus margin, minus the time
  since). The file contains **no time of day**. The watchdog polls it every 250 ms, remembers on its own monotonic clock
  when it last saw `seq` change, and when `V` has passed since then it writes the fence marker and stops the instance
  with an immediate shutdown. It exits when the instance is no longer running (the pid in `postmaster.pid` is gone) or
  after the fence. It starts at the next renewal after the group counts as enforced (a standby appeared), so there is a window of at most `lease-renew` in which a frozen kingresd is not yet covered by it. It is looked for in `pg-bin-dir`, then next to the kingresd binary; without it kingresd warns once
  and only the in-process fence protects the group.
- **Views.** `GET /replication/topology` shows, per group, `lease` (`holder`, `state`, `remainingMillis`, `enforced`) as
  the answering server knows it: `held` or `lost` for the group whose primary it runs, `granted` (holder and time left)
  for a group it granted to another server. `GET /internal/v1/lease/state` shows the same with the grants.

#### Why this is safe (the two timelines)

Let `D` be `lease-duration`, `M` `lease-margin`, `s` the instant a renewal round started on the holder and `r >= s` the
instant a grantor of the majority received its request (the request is sent after `s`). The holder stops acting on the
lease at `s + D - M` on its clock and has stopped the instance moments after (an immediate shutdown does not wait for
clients; the check interval of 100 ms and the stop itself must fit in the margin). The grantor refuses every other holder
until `r + D + M` on its clock. A new holder needs a majority, and any two majorities share a server, so a new holder
cannot be granted before `r + D + M`; the old holder's last act is at about `s + D - M`. Between them lies at least
`2M` (6 s with the defaults) minus the difference in the speed of the two clocks over `D` and the scheduling delay of the
check: the margin must be larger than both (it is a setting). The first timeline is measured by the tests: the last
write the old primary acknowledged against the earliest instant at which the grantors of the majority would grant the
group to another holder (`TestPartitionedPrimaryWithStandbyStopsBeforeANewHolderCouldBeGranted`: with `D=6s, M=2s` the last
write was acknowledged 3.4 s after the cut and the lease was free 7.4 s after it).

A holder that is paused (SIGSTOP, a stalled virtual machine) and resumed does not trust its past: every judgement uses
the monotonic clock, which keeps running while the process is stopped, and a lease obtained by a round that started
before the pause is valid from that start only, so answers that arrive late give a lease that is already over. While it
is paused the watchdog does its job. A step of the system clock does not matter because no decision reads it
(`clock.Clock.Mono`); the tests step the wall clock of a server through the fault hook and see nothing change.

#### Settings (flags, environment, file keys)

| Flag / file key | Environment variable | Default | Meaning |
|---|---|---|---|
| `lease-duration` | `KINGRES_LEASE_DURATION` | `10s` | validity of a lease from the start of the round that obtained it |
| `lease-renew` | `KINGRES_LEASE_RENEW` | `2s` | how often the holder renews (at most half of duration minus margin) |
| `lease-margin` | `KINGRES_LEASE_MARGIN` | `3s` | safety margin: the grantor waits it after expiry, the holder fences that long before |
| `fault-dir` | `KINGRES_FAULT_DIR` | empty (off) | **tests only**: directory polled for `faults.json` (package `fault`): blocked servers in each direction, a wall-clock offset, and `forceEnforce`, which makes every group count as having a standby |

#### Decisions (leases)

1. The **fence marker** is a file, not state in memory: a start gate in the instance manager reads it, so that no path
   (the start operation, kingresd's start, a restart of the machine) starts the primary of a fenced group.
2. A restarted `kingresd` judges nothing until its first renewal round has finished, so a primary that kept running
   while kingresd was restarted is not stopped before the first round could succeed. The window is at most one round
   (`lease-duration - lease-margin`); a primary that the watchdog stopped in the meantime stays stopped.
3. The **majority** is counted over every distinct host in `kingres.node` (read from the local catalog, cached in
   `<data-dir>/lease/topology.json` for the time the instance is down), active or not: a dead server is still a member.
   A change of one server changes the set by one, and the majorities of the old and new sets intersect. A server that
   is being added has no entry before its registration step.
4. The grantor **persists only a change of holder** (or a longer duration); renewals by the same holder are memory only.
   After a restart the persisted duration is assumed to start now, which is never earlier than the truth.
5. The lease views are **this server's knowledge**, not a gathered cluster view.
6. The watchdog is started **next to** the instance by kingresd (see the departures).
7. **A primary whose lease is enforced is not started by kingresd before kingresd holds the lease.** At its start
   kingresd finds the instance either running (it was restarted, the database kept running: rule 2 applies, nothing is
   judged before the first round and a healthy primary is not stopped) or not running (the orderly stop of the whole
   cluster, a reboot, a crash). In the second case, when the lease of the group is enforced, kingresd writes the
   **wait marker** `<data-dir>/lease/waitlease` and does not start the instance; the start gate (decision 1) refuses it
   like it refuses a fenced one. The first renewal round that wins a majority removes the marker and starts the instance
   (once; the start runs beside the next rounds, so the lease is renewed while PostgreSQL starts or recovers). Whether a
   group is enforced must be known without the database (the map group's server cannot read the catalog before its
   instance runs), so the lease manager records what it last knew in `<data-dir>/lease/enforced.json` (`{"groups":[2]}`, next
   to `topology.json`), rewritten whenever a successful round finds the answer changed; no record means not enforced, and
   a group that is not enforced starts at once as before. The wait marker is **not** an operator's stop: that one lives in
   memory, writes no marker, and an instance stopped through `stopThisServerInstance` stays stopped when a lease is
   acquired (only the fence marker and the wait marker make the lease manager start anything). `kingresd -stop-instances`
   (the orderly stop of the service) leaves exactly the state "not running"; the next start of kingresd turns it into the
   wait marker, so "start it at the next start of kingresd, once the lease is held" needs no write by the stop (and a
   running kingresd, which `ExecStop` of the unit leaves alive meanwhile, never restarts what was stopped). While the
   instance waits: the first round that fails fences nothing and writes no audit event (`leaseLost`, `leaseFenced`), the
   alert `leaseLost` is not raised (a group that never held the lease in this run is judged only once it has held it),
   the lease shows as `none` in `GET /internal/v1/lease/state` (and in `ShardGroup.lease` of any server that can answer: the
   waiting server itself cannot read its catalog, so its REST calls answer 503 as for a held-back server, decision 12
   of the cluster section), and a failed start puts the marker back and is tried again by the next round. A standby
   instance that this server runs for another group is never held back by these markers (it follows its own primary's
   lease, not this server's). Tests: `internal/lease` (`TestColdStartWithoutAMajorityStartsNothingAndFencesNothing` and the
   four after it, fake clock) and `internal/clustertest/coldstart_test.go` (`TestRestartsOfServersDoNotCrashOrFenceAnInstance`:
   three servers with standbys, kingresd alone restarted, one whole server stopped and started, and all three stopped
   together and started one by one with a pause between; the PostgreSQL logs have no `immediate shutdown request` and
   no `automatic recovery in progress`, every primary runs and holds its lease, a write through each succeeds, the audit
   logs have no `leaseFenced`).
8. **An orderly stop tells the watchdog to stand down.** The container's entry point stops kingresd first (no more
   renewals) and only then runs `kingresd -stop-instances`; a fast shutdown can last longer than the lease (a slow
   checkpoint, a backend that does not end, a walsender that must send its WAL), and the watchdog, which outlives
   kingresd, then stopped the instance with an immediate shutdown in the middle of the clean stop (measured by
   `TestOrderlyStopOutlastingTheLeaseIsNotFencedByTheWatchdog`: a stopped backend keeps the shutdown waiting for 12 s with
   a lease of 6 s; the watchdog fenced after about 4 s). `-stop-instances` therefore asks the primary instance for the fast
   shutdown without waiting (`pg_ctl stop -m fast -W`), and **after** PostgreSQL has accepted the request writes
   `<data-dir>/lease/standdown` with the process id and start time of that postmaster (lines 1 and 3 of `postmaster.pid`),
   and only then waits. From the request on PostgreSQL admits no new session and ends the existing ones, so no write can
   be acknowledged after the lease ends. The watchdog exits when the file names the postmaster that runs now; a file of
   an earlier run names another one and is ignored. Only the `-stop-instances` path writes it, so a hung or killed
   kingresd, or a crash of the instance, is still covered by the watchdog. Residual window: from the exit of kingresd to
   the request, which is the start of a process that checks nothing (the check of the PostgreSQL programs is skipped in
   this mode); the lease still has `lease-duration - lease-margin` minus one renewal interval then (5 s with the defaults).
   The unit of the package (`ExecStop` while kingresd still runs) has no such window, because kingresd renews the lease during
   the stop; it stays `ExecStop` (an `ExecStopPost` would also run when kingresd alone is restarted) and got a comment.
   Test of the stand-down in the watchdog: `cmd/kingres-watchdog` (a fake `pg_ctl`: fences without the file, stands down with it, ignores a stale one).

Limits: a lease-based fence cannot stop a primary on a machine that is frozen as a whole and then resumed with the same
processes in the middle of a write that was already acknowledged to the kernel; and the guarantee needs `M` larger
than clock-rate error and scheduling delay (stated above). Not done in this part: nothing grants a lease to another
server (failover is part 2); `leaseLost` for an enforced group is raised only while its instance can store alerts, the
`serverDown` alert covers the rest.

### Standbys

Package `internal/standby`, design `ha-phase5.md` part A, without the sharding catalog (the installed extension is 0.2;
see "What part 2 must add" below). Every shard group has one primary and `defaultReplicaCount` standbys (0 to 3; the
setting is `replica.default` in `kingres_cp.cluster_setting`, ADR 0011).

**What exists**

- **Placement** (`placement.go`, pure functions): the standbys of the group whose primary runs on server *i* go to the
  next servers after *i* in the ring (the servers in the order of their groups, wrapping), skipping servers of the
  primary's server group while other choices exist and preferring a server group no earlier standby of the group
  uses. A server never runs two instances of one group, so with fewer servers than `replicaCount + 1` the group has
  fewer standbys (it is under-replicated). Placement is stable: a standby that works stays where it is when servers
  join; lowering the count removes the least preferred.
- **The task `standby.reconcile`** (subject `replication` / `standbys`, at most one at a time; the kind is not in the
  contract's `TaskKind` list until 0.7.0): the leader checks every 10 s (and at once when `defaultReplicaCount` is patched,
  even to the same value) whether the standbys match the catalog and the setting, and starts the task with the work it
  found. The steps are, group by group: remove standbys, create standbys, rebuild standbys, "Send the list of standbys to
  every server", and `standby.register_in_catalog`. A failed create or rebuild is tried again after 30 s (not a setting).
- **Creating a standby** (target server, `POST /standby/create`): the leader first records the standby in
  `kingres_cp.standby` and sends the list (the standby not yet ready) to every server, so the primary's server knows it;
  the target then creates the **physical slot** `kingres_standby_<id>` on the primary (`pg_create_physical_replication_slot`
  with immediate reservation, over a certificate-authenticated connection), takes the base backup with
  `pg_basebackup -X stream -S <slot>` over the **replication connection authenticated by the server certificate** (no
  password), writes the managed configuration of the standby (a copy of the primary instance's with a free port,
  `hot_standby`, `primary_conninfo` with the certificate options, `primary_slot_name`, application name
  `kingres_standby_<id>`), writes `standby.signal` and starts it as an instance of its own in
  `<data-dir>/instances/<100 + group>`. The record becomes `ready` and the list is sent again. Every part checks what
  exists, so a repeated task step continues (a base backup without its metadata file is thrown away and made again).
- **Replication rules in `pg_hba.conf` and `pg_ident.conf`**: next to the existing `hostssl all all <cidr> cert map=kingres_nodes`
  lines there is now `hostssl replication all <cidr> cert map=kingres_nodes` for every peer network (`all` never matches a
  replication connection). The ident map is the existing one: the certificate name of any server maps to any role. A standby
  has the same two files as the primary instance of its server (`Manager.SyncAccess`, checked every round) plus a rule that
  lets kingresd in over the private unix socket by peer authentication: a standby is a copy of another server's data, so its
  service role has that server's password, not this server's.
- **Supervision** (`Agent.Run`, every server, every 3 s): the standby instances of this server are started when they are not
  running (a restart of kingresd or of the machine resumes them), their access files follow the primary instance's, a changed
  server certificate, CA or revocation list makes them reload, and a standby that the list no longer names (removed while its
  server was unreachable) is deleted.
- **The primary's side** (same loop, on the server that runs a group's primary, no leader needed): slots exist for the standbys
  of the list and for no others (an inactive slot that belongs to no listed standby is dropped); a slot that retains at least
  `slot-retention-warn-bytes` raises `replicationLagHigh` (subject `server:<s>|group:<g>|standby:<id>`); above
  `slot-retention-limit-bytes` the slot is **dropped** (audited `replicationSlotDropped`) and the standby is marked as lost;
  a ready standby whose slot is missing or `lost` is marked the same. The leader reads the marks (`GET /standby/status`, every
  check) and rebuilds those standbys (`POST /standby/rebuild`: the instance is deleted, a new slot and a new base backup are
  made; with `diverged` set `pg_rewind` is tried first and the new base backup is the fallback; nothing in this phase sets it,
  the promotion of part 2 does).
- **`synchronous_standby_names`** (`syncnames.go`, `SyncState.Decide`): `ANY 1 (<all the group's ready standbys>)` as soon as
  at least one of them is connected and streaming; empty for a group without standbys. When no standby streams, the setting is
  left alone for `standby-sync-grace` (default 30 s: a standby that reconnects within it never lets commits through without
  it); after it the setting is emptied so that the group keeps accepting writes, `groupWithoutSynchronousStandby` is raised and
  stays until a standby streams again, when the setting is put back and the alert clears. It is never emptied while the hook
  `Agent.Frozen(group)` says that a failover of the group is being decided (no failover exists yet). A standby that has not
  streamed yet is not put in the setting, so a new standby never makes its group wait during the base backup. This is the
  trade of the design: commits are acknowledged by two servers while a standby streams and by one after the grace period,
  which the alert says in those words. The setting lives in `kingres-managed.conf` (`Extra`), not in `ALTER SYSTEM`.
- **Alerts**: `groupWithoutSynchronousStandby` (per server and group, on the primary's server), `groupUnderReplicated` (cluster
  scope, `cluster|group:<g>`, raised by the leader when fewer standbys stream than the replica count at three checks in a
  row; never in a one-server cluster, where nothing can be placed), and `replicationLagHigh` for slot retention. The first
  two kinds are not in the `AlertKind` enum of contract 0.6.0 (0.7.0 adds them).
- **Views**: `getReplicationTopology` lists the standbys kingresd manages in the `standbys` of their group, with the
  synchronous state and the lag the primary reports for the walsender of the same application name; a group whose standby
  does not stream is `degraded`. Their `instanceId` is `1048576 + standby id` (they have no catalog node id yet). Every
  server holds the whole list, so every server answers the same.
- **Removing a server** (`removeServer`, `leaveCluster`): a step "Remove the standbys of the server" runs after the checks and
  before the server is unregistered. It deletes the standbys that run on the server and the standbys of the group whose
  primary it runs, with their slots; the server that goes also deletes its standby instances when it is reset
  (`Service.RemoveInstance`). The groups that remain place their standbys again.
- **Persistence**: the leader's record is `kingres_cp.standby` (migration `0006_standby.sql`, schema version 6, plus the
  sequence `kingres_cp.standby_plan_version`). Every server keeps the list the leader sent in
  `<data-dir>/standby/state.json` (fsync and atomic replace) and a metadata file `kingres-standby.json` in the directory of
  each standby it runs. An older list never replaces a newer one, also not after a change of leader (the version is a
  sequence of the system database).

**Configuration keys** (in `internal/config/ha.go`; flag, `KINGRES_*` environment variable and file key as for every setting)

| Key | Default | Meaning |
|---|---|---|
| `standby-sync-grace` | `30s` | how long no standby may stream before the group stops waiting for it |
| `slot-retention-warn-bytes` | `1073741824` | WAL a standby's slot may retain before `replicationLagHigh` is raised |
| `slot-retention-limit-bytes` | `4294967296` | WAL a standby's slot may retain before it is dropped and the standby rebuilt |
| `standby-port-base` | `pg-port + 1000` | first port tried for a new standby instance (the first free one from there is used and recorded) |

The replica count itself is the cluster setting `defaultReplicaCount` (`PATCH /settings/cluster`).

**Tests.** Unit: `internal/standby` (`TestCandidates*`, `TestPlan`, `TestSyncNamesRule`, `TestSlotPolicy`),
`internal/instance` (`TestNodeRulesCoverReplicationConnections`, `TestStandbyDirIDs`), `internal/sysdb` (version 6).
Multi-server, in WSL (`internal/clustertest/standby_test.go`): `TestStandbysAreCreatedForEveryGroupAndStream`,
`TestReplicaCountRaisedAndLowered`, `TestSynchronousCommitNeedsTheStandbyUntilTheGracePeriod`,
`TestStandbysSurviveRestartsAndCertificateRenewal`, `TestRemovingAServerRemovesItsStandbys`.

**Decisions where the design was silent**

1. The leader pushes the whole list of standbys to every server (`POST /standby/plan`), not only to the primary's server: the
   replication view and `HasStandby` (which decides whether a lease is enforced) must work on every server without the leader.
2. Standby instances are the directories `100 + group` of the instance manager; the port is the first free one from `standby-port-base` (default `pg-port + 1000`; not
   `pg-port + 1`, which on a one-machine cluster is the port the next server will bind), recorded in the standby record.
3. The standby's server creates the slot on the primary itself (one call to the target), over the same certificate
   connection as everything else; the primary's own loop only maintains and drops.
4. A lost or dropped slot is never recreated in place (the new one would start at the current position and the standby needs
   older WAL): the standby is rebuilt from a new base backup.
5. `synchronous_standby_names` is owned by the standby agent in the managed file; it names all ready standbys (`ANY 1`), not
   only the streaming ones, so that a standby that falls behind stays a candidate of the quorum.
6. A one-server cluster raises no `groupUnderReplicated` (it is the development arrangement). With two servers or more it does,
   also when fewer standbys can be placed than the count asks for; the message says so.
7. Standby instances are not stopped by a stop of kingresd (like the primary instance); `-stop-instances` stops them first.
8. kingresd reaches a standby by peer authentication on its private unix socket, never by a password.

**Limits**: a cluster created before this version has no replication lines in `pg_hba.conf` until its trust rules are written
again (any change of servers or network rules does it); `pg_rewind` is only a speed-up, the fallback is a full copy; the
server and instance lists show catalog instances only (the standbys are in the replication view).

### The client entry point

Package `internal/clientproxy`, started by `cmd/kingresd/ha_proxy.go`. **Off unless configured** (the packaging
decides the ports later): with neither key below set nothing listens.

| Flag / file key | Environment variable | Default | Meaning |
|---|---|---|---|
| `client-listen` | `KINGRES_CLIENT_LISTEN` | empty (off) | host:port of the primary entry point |
| `client-standby-listen` | `KINGRES_CLIENT_STANDBY_LISTEN` | empty (off) | host:port of the entry point that forwards to a local standby for lag-tolerant reads |
| `client-max-connections` | `KINGRES_CLIENT_MAX_CONNECTIONS` | `1000` | connections forwarded at once, both entry points together; one more is closed at once and counted |
| `client-connect-timeout` | `KINGRES_CLIENT_CONNECT_TIMEOUT` | `5s` | time to choose the destination and connect to it |
| `client-idle-timeout` | `KINGRES_CLIENT_IDLE_TIMEOUT` | `1h` | a forwarded connection with no byte in either direction for that long is closed |

**What it does.** It accepts a TCP connection and connects it to a destination it chooses, then copies bytes in both
directions, from the first byte. It never reads, parses, buffers or logs what the client sends: the startup packet, an
`SSLRequest`, TLS, SCRAM, a client certificate and SQL are all just bytes. TLS is therefore negotiated end to end between
the client and the PostgreSQL instance, and authentication is PostgreSQL's own. The proxy sends nothing of its own (a
connection that cannot be forwarded is closed without a message: there is no PostgreSQL error message it could send
without first speaking the protocol).

**Destination of the primary entry point**, chosen for each new connection and looked at again every second for the open
ones (a connection whose destination differs from the one chosen now is closed, and counted):

1. the primary instance of this server (on the loopback address), when the shard catalog (this server's copy) says it is
   active and this server holds the valid lease of its group (`ha.Leases().Holds`). When no lease manager is running
   (it reports no lease at all) the catalog alone decides: there is nothing that could say otherwise;
2. otherwise the client entry point, on the same port number, of the server that runs the next active primary after this
   server's group in group order (a ring), skipping servers that the heartbeats say are dead. This is **not** the
   instance of that server: an instance accepts a connection from another server's address only with that server's
   certificate (the node rules at the top of `pg_hba.conf`, a `cert` rule that does not fall through), so a user
   forwarded to it directly would be refused. The entry point of that server connects to its own instance from its
   loopback address. The consequence is that every server of the cluster must run its entry point on the same port
   (that is what the packaging sets). A connection that comes from the address of another server of the cluster is
   never forwarded a second time (no loops): it is served by the local primary or closed.
3. nothing: the connection is closed.

**TLS names.** Because TLS ends at the instance, a client that dials server A and is forwarded to server B sees B's
certificate, which names B. A client that verifies the host name (`sslmode=verify-full`) refuses it; it must verify the chain
only (`verify-ca`) or be given a certificate that carries every server's name (not issued today). Connections that stay on
the server that was dialed (the normal case, while that server holds its lease) are unaffected.

**The standby entry point** forwards to a running standby of this server's own that kingresd manages (a streaming one
first, otherwise a running one that is not: the client asked for reads that tolerate lag), on the loopback address. Until
the standby manager reports one it closes the connections.

**What the instance sees (`pg_hba.conf` address matching).** The instance sees the address of the proxy's connection, not the
client's: `127.0.0.1` for the local destination (and for the second hop of a forwarded connection). PostgreSQL has no way
to take the client's address from a proxy (there is no PROXY protocol support in stock PostgreSQL, and a proxy that
does not read the stream cannot pass it anywhere else), so:

- the managed network rules (`replaceNetworkAccess`, `internal/ops/network.go`) are matched against that address for
  proxied clients. A rule that names the client's network (`10.1.0.0/16`) does not match a proxied client; the default
  rules (`127.0.0.0/8`, `::1/128`, password) match every proxied client, which is why proxied clients can sign in on a
  fresh cluster. To let proxied clients in, a rule must include the proxy's own address (`127.0.0.0/8` here), and that
  rule then admits every client of the proxy, whatever network it is on;
- address rules therefore no longer say anything about who may connect to the entry point. Limiting it is the job of
  the address `client-listen` binds (a private interface), of the host firewall, and of roles, passwords, SCRAM and
  client certificates, which are end to end and unaffected. A rule of type `reject` for a client network cannot be
  enforced through the proxy;
- `pg_stat_activity.client_addr` shows `127.0.0.1` for proxied sessions. The one exception is a cluster on a single machine
  (every server host is a loopback address, as in the tests): there `127.0.0.1` is already listed in the instances' node
  rules as a source of the other servers (a certificate is required from it), and so is each server's own address, so the local leg then comes from
  `127.255.255.254`, a loopback address no instance lists (the default `127.0.0.0/8` rule admits it);
- the local leg is TCP on the loopback address, not the unix socket. TLS is not available on a unix socket (the server
  answers `N` to an `SSLRequest`), so a client that requires TLS could not connect, and the `local` rules (`peer map`)
  are for kingresd's own connections as signed-in roles and must not become reachable by clients.

**Metrics.** Counters since the start of `kingresd`, written to the metrics ring every five seconds and shown by `/metrics`
as `kingres_client_connections_accepted`, `_active` (a gauge), `_refused` (over the cap), `kingres_client_connect_failures`,
`kingres_client_connections_moved` (closed because the destination changed), `kingres_client_connections_idle_closed`,
`kingres_client_bytes_in`, `kingres_client_bytes_out`, and the same with `kingres_client_standby_` for the second entry
point. Event lines are logged at debug level and carry no address and no data. They are not in the contract's `MetricName`
enum yet (see the notes for 0.7.0).

**Tests.** `internal/clientproxy` (unit: bytes unchanged both ways including an `SSLRequest` first and random noise, the proxy
never speaks first, cap and shared cap, connect and idle bounds with an injected clock, closing on a destination change,
destination rules with fakes, no forwarding loop); `internal/clustertest` `TestClientEntryPoint` (three real `kingresd`: TLS
and SCRAM through every entry point lands on that server's instance with no client certificate recorded; a client
certificate signed by the cluster CA authenticates through it; a destination change closes the old connections and sends new
ones to another server and back).

**Limits.** The destination is looked at every second and a connection is closed, not moved (a client reconnects; it is
the same as a failover for a client). Which primary a connection gets when the local one is unusable is the next server of
the ring, not the one with the least load. The loop rule recognises a forwarding server by its address in the peer
directory, so a forwarding hop that arrives from another source address is served like a client (it is then forwarded once
more, at most until the destination holds the lease).

### Cancelling a workbench statement

Engine in `internal/ops/cancel.go`, HTTP operation `cancelStatement` since contract 0.7.0: `executeQuery` takes an optional `statementId` (1 to 64 characters of `A-Za-z0-9_-`, chosen
by the client so that it is known before the answer arrives), and `POST /query/statements/{statementId}/cancel` (role
`kingres_readonly`, audit action `cancelStatement`) answers 204, or 404 `not_found` when the signed-in role has no such
statement running on this server.

- The server tracks each statement it runs, keyed by the signed-in role and the id, with the backend pid of the connection
  kingresd itself opened for it. A pid never comes from a client. A role can only find its own statements; a second
  running statement with the same id and role is 409 `conflict`.
- A cancel marks the statement, runs `pg_cancel_backend(pid)` on a service connection restricted to that pid, role and
  database, and starts a two-second safety net that ends the statement from this side if the server did not (a cancel that
  reaches a backend between two statements is lost). The statement ends with SQLSTATE `57014` ("canceling statement due to
  user request"), a 422 `sql_error` like the statement timeout.
- The entry is marked done under its own lock **before** the connection is closed, and a cancel holds that lock while it
  runs, so a late cancel can never reach a backend number that PostgreSQL has handed to another session.
- `executeQuery` runs on the server that received it (it is not forwarded to the leader), so the cancel is local too: a
  session is on one server (sessions are per server), and a cancel sent to another server is 404. It is not forwarded.
- Tests: `internal/ops` `TestCancelFindsOnlyTheCallersOwnRunningStatement`, `TestCancelAfterTheStatementEndedNeverCancels`,
  `TestIntegrationCancelEndsARunningStatement` (real instance: a `pg_sleep(60)` as a plain role is cancelled by id, SQLSTATE
  57014, another role cannot cancel it, a running id conflicts, the id and the slot are free afterwards).

### Carry-overs (workbench cancel, startup race, test ports, task fencing)

**The `403 not_leader` on `/peers` (startup race).** The leader resolver (`internal/leader`) cached every answer
for 2 s, also the answer "the catalog names no leader" with no error. A server that has just been added has an empty
catalog until the leader copies it, and its task loop asks for the leader every 2 s, so the empty answer is
cached right up to the moment the leader's last calls arrive (`/node/finish`, then `POST /peers`, policy
`LeaderOrJoining`, which is no longer "joining" once the server is clustered): `Leader()` returned "" from the cache and
the call was refused with 403 `not_leader` for up to 2 s. Two changes: an empty answer is **never cached**
(`leader` `TestNoLeaderIsNotCached`, which fails without the change), and the internal API asks again, up to three
times 200 ms apart, before it refuses a leader-only call because the leader is unknown
(`internalapi` `TestUnknownLeaderIsAskedAgainBeforeARefusal`; this also covers a catalog that is read slowly under
load, where `Current()` gives up after 3 s and answered ""). A joining server is no longer asked at all (it never
knows a leader). Evidence: the failure was seen once; with the old code a loop of
`TestCertificateRenewalKeepsAThreeServerClusterWorking` ran 6 of 6 green (about 80 s each), so the race is **proved by
reading and by the unit tests, not reproduced in the loop**; the same loop on the fixed tree ran
6 of 6 green too (about 74 s each, same shared machine).

**Test infrastructure.** `scripts/run-wsl-tests.sh` gives every run a root `~/kw-cp/test/run-<pid>-<epoch>` and a slice
of 200 ports (`20000-27999`, below the kernel's range for outgoing connections; `KINGRES_TEST_PORT_BASE/END`), claimed
with an atomic `mkdir` under a `flock`, so two runs never choose the same port and the port of
`TestFailedJoinBeforeRegistrationCleansUp` cannot be taken by another run (`pgtest.FreePort` also probes the
wildcard address now, because the test itself binds `:port`). The run exports `KINGRES_TEST_ROOT=<root>`; everything
the tests start inherits it. Each run writes a ledger entry (script pid, its start time, root); the next run (or
`run-wsl-tests.sh --reap`) reads the ledger, and for every entry whose script is gone (pid and start time do not match
any more) stops the processes whose environment contains exactly `KINGRES_TEST_ROOT=<that root>` (TERM, then KILL
after 20 s), removes the root and frees the slice. Nothing is identified by a name pattern, other people's processes
and runs never carry that variable and are never touched. Verified by a run whose script was killed with SIGKILL while
three `kingresd` and three instances ran (the next run removed them and the root), and by a simulated leftover next
to two decoy processes (one without the variable, one with another root) that survived. `run-integration.ps1` takes
`-Count N` (every selected test N times, for stress loops), builds `cmd/kingres-watchdog` when the directory exists
and exports `KINGRES_WATCHDOG_BIN`. `wsl-allowed-skips.txt` stays empty.

**Fencing of remote task calls (`internal/fence`).** The task engine puts the run's token (task id and lease epoch) into
the context of every step (`fence.With`); `peerclient` sends it as the header `X-Kingres-Fence: <task>:<epoch>` on every
call made with that context; the callee (`internalapi`) checks it for calls that come from the leader (leader-only
policies) against a registry of the highest epoch seen per task and answers `409 fenced` for a lower one. Equal or
higher is accepted and recorded (on disk, `<data-dir>/fence.json`, fsync and atomic replace, before the call is
accepted), a callee never lowers what it knows, and a restarted callee reads the file, so a superseded run cannot get
through just because the callee restarted; a damaged file starts empty with a warning (refusing every call would stop
all tasks, and the store's epoch check remains). At most 4096 tasks are remembered (the oldest ids are dropped).
Calls without the header, and every header from a server that is not the leader, are not fenced (a member cannot
raise an epoch to block the leader). Tests: `fence` unit tests, `internalapi` `TestSupersededRunIsRefused`, `task`
`TestStepContextCarriesTheFence`, clustertest `TestSupersededTaskRunIsRefusedByTheCallee` (three servers, the late
epoch 1 call refused after epoch 2 touched the callee, also after the callee's kingresd was restarted). Limit: a late
call to a callee that the newer run has not called yet is accepted; for what the step writes in the system database
the store's epoch check refuses it.

### Decisions of this increment

1. **Heartbeats are all-to-all and measured on the receiver** with a monotonic clock; nothing but durations ever crosses the wire. `dead` and `suspect` are the answering server's view.
2. **A follower distrusts its cache after `leader-loss-bound` (30 s)** for session-authorized requests (503 `leader_unavailable`); a shorter blip changes nothing, probes and the leader itself are exempt.
3. **Self-fencing is enforced only for a group that has a standby** (decided by the coordinator); a group without one only raises `leaseLost`. A one-server cluster grants itself.
4. **The watchdog is started by kingresd next to the running instance**, not as its parent, so every existing start, stop, restart, reload and leave path is unchanged. It watches a sequence number, not a time of day.
5. **The cross-server leg of the client entry point goes to the other server's entry point**, not its instance (the instances refuse client addresses of peers); the local leg is TCP loopback because TLS is impossible on a unix socket. The instance therefore sees a proxy address (see "The client entry point").
6. **Cancel is local to the server that runs the statement** and is not forwarded; a client chooses the statement id; no pid comes from a client.
7. **`workbench_busy` is 429** for both the per-role and the server-wide cap.
8. **Standbys**: whole standby list pushed to every server; local instance directory id `100 + group`; a lost slot means a rebuild; `defaultReplicaCount` of `initCluster` is 1, so every cluster of two or more servers now gets standbys; the removed server's own catalog being empty is covered by remembering the last verified leader for 60 s (`leaderMemory.recent`), because the two-second cache that used to cover it was shorter than the removal of the standbys.
9. **A leader answer of "none" is never cached** and the internal API asks again up to three times before refusing a leader-only call (the `/peers` race).
10. **Fencing epoch** is checked only on leader-only policies, persisted in `fence.json`.
12. **A server whose instance is held back until its group lease is won answers 503, not 403.** After kingresd was down long enough for the watchdog to fence the instance, the restarted kingresd cannot read the catalog until the lease round has finished and the instance has started, so it does not know the leader. In that window (the fence marker exists, or the instance is not running, and no leader is known) the internal API answers `503 service_unavailable` with `Retry-After: 2` instead of `403 not_leader`; the REST sign-in answers `503 service_unavailable` with `Retry-After: 5`, as it already did. A task step (its context carries the fencing token) retries that answer with a growing pause (1 s to 8 s) for up to 5 minutes (`peerclient.NotReadyBound`) and only then fails, with an error that names the server and says it waited for its group lease; a request of a user is not retried. Test: `internal/clustertest` `TestTaskWaitsForAServerThatWaitsForItsLease`.
11. **Test ports** are a per-run slice of 20000-27999; cleanup kills only processes whose environment names the run's root.

### Departures from ha-phase5.md

- The design says a fenced primary stays down until it holds a lease "or has been told it is now a standby": only the first half exists (the second is part 2).
- The watchdog is not the parent of the instance (decision 4).
- A restarted kingresd judges nothing until its first renewal round has finished (at most one round without re-confirmation).
- The setting is named `defaultReplicaCount` (the contract's name), not `replicaCount`; the ring order is join order.
- `synchronous_standby_names` names all *ready* standbys and is non-empty only once one streams, so a standby in its base backup never blocks the group.
- `pg_rewind` exists but nothing calls it with `diverged` yet (no failover).
- The client entry point's cross-server leg goes through the other server's entry point (decision 5).
- The wall-clock step test shifts the wall clock inside kingresd (fault hook); a step of the system clock needs root and was not made.

### What part 2 must add

- **Catalog registration of standbys:** implement `standby.CatalogRegistrar` (`internal/standby/catalog.go`: `Register`, `Unregister`) with kingres_shard 0.4 and pass it as `Leader.Registrar` in `newStandbyParts` (`cmd/kingresd/ha_standby.go`) instead of `SkipCatalog{}`. The task step is `standby.register_in_catalog`; store the node id in `kingres_cp.standby.catalog_node_id` and drop `pgstore.ManagedStandbyInstanceBase`.
- **Promotion:** set `Agent.Frozen` during a failover decision; obtain the lease through `POST /lease/grant` (use `dryRun` to wait for the old lease); make `Agent.maintainPrimary/Status/Create`, `catalogTopology` and `localInstance` in `ha_leases.go` and the watchdog files use the instance id of the group's primary instead of instance 1; repoint the other standbys (`primary_conninfo`); rebuild the old primary with `POST /standby/rebuild` and `diverged: true`; remove `lease.FencedMarker` when a server becomes a standby.
- **Automatic failover** reads `ha.Liveness.State/Silence` (count `autoFailover.timeoutSeconds` from the dead transition) and `Leases`.
- The 60-second leader memory (decision 8) must be revisited with leader moves.

---

Copyright (c) 2026, RooyeKhat Media Co. Released under the PostgreSQL License.
