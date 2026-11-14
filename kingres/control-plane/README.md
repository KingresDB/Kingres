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
[Running several servers](#running-several-servers)). Rebalance, failover, restoring preferred primaries, server
groups, cluster-wide logs, alerts and metrics, the certificate renewal job and leaving a cluster still answer the
contract's "not supported yet" problem. Design: `kingres/docs/design/control-plane-2.md` and
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
manager itself in a throwaway directory under `~/kw-cp/test`, on ports 56000-56199, and removed afterwards.

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
| `allow-remote-setup` | `KINGRES_ALLOW_REMOTE_SETUP` | `false` | `true` lets the setup operations come from non-loopback addresses while uninitialized (containers listening on `0.0.0.0`); the setup token is still required |
| `-version` | | | prints version, contract revision (0.3.0) and build commit (`-ldflags "-X main.commit=..."`, else `unknown`) |
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
bodies.

## What is implemented

The server implements revision **0.3.0** of the contract (86 operations, 69
paths). "Real" means the operation works against PostgreSQL or the local node
state. "Not yet" means: after authentication, CSRF and role checks have passed
the server answers **HTTP 422 with `code: unsupported_operation`**
(`Operation <id> is not supported by this server version yet.`), which every
authenticated operation lists in 0.2.0. **67 of 86 operations are real; 19 answer "not yet".**

| Tag | Real | Not yet |
|---|---|---|
| Alerts | `listAlerts`, `acknowledgeAlert` |  |
| Audit | `listAuditEvents` |  |
| Auth | `login`, `logout`, `getSession` |  |
| Cluster | `getCluster`, `getClusterSummary` |  |
| Roles | `listClusterRoles`, `listRoles`, `createRole`, `getRole`, `deleteRole`, `updateRole`, `setRoleClusterRoles`, `setRoleMemberships` |  |
| Databases | `getDatabaseCreateOptions`, `listDatabases`, `createDatabase`, `getDatabase`, `dropDatabase`, `updateDatabase` |  |
| Shards | `getBucketDistribution`, `listColocationGroups`, `listBuckets` |  |
| Ledger | `getLedgerStatus`, `listLedgerBlocks`, `listLedgerDigests`, `generateLedgerDigest`, `startLedgerVerification`, `listLedgerStatus` | `createInclusionProof`, `listLedgerVerifications`, `getLedgerVerification` |
| Tables | `getTable`, `distributeTable`, `enableTableLedger`, `listTables` |  |
| Grants | `listGrants`, `createGrant`, `revokeGrants`, `applyGrantTemplate` |  |
| Health | `getHealth`, `getReadiness` |  |
| Logs | `listLogs` |  |
| Metrics | `queryMetrics` |  |
| Query | `executeQuery` |  |
| Rebalance |  | `getRebalanceStatus`, `startRebalance`, `previewRebalancePlan`, `listRebalanceReports`, `getRebalanceReport`, `stopRebalance` |
| Replication | `getReplicationTopology` | `restorePreferredPrimaries` |
| Server groups | `listServerGroups` | `createServerGroup`, `deleteServerGroup` |
| Servers | `listServers`, `getServer`, `addServer`, `removeServer` | `updateServer`, `failoverServer` |
| Settings | `getClusterSettings`, `updateClusterSettings` |  |
| Network access | `getNetworkAccess`, `replaceNetworkAccess` |  |
| Setup | `initCluster`, `joinCluster`, `getNodeState`, `checkSetupToken` |  |
| Tasks | `listTasks`, `getTask`, `cancelTask` |  |
| This server | `getThisServer`, `listThisServerInstances`, `getThisServerInstance`, `restartThisServerInstance`, `listThisServerLogs`, `getThisServerResources` | `startThisServerInstance`, `stopThisServerInstance`, `leaveCluster`, `getThisServerSettings`, `updateThisServerSettings` |

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
and `revoked.json`. The renewal job is not written yet (`NeedsRenewal` exists): a certificate has to be replaced
before it expires after a year, and the CA is not rotated.

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

**Pinned fingerprint or trust on first use.** The first contact has no common CA. The internal API of a server
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
(serial numbers in hex, with the time). Removing a server revokes its certificate. There is no public operation to revoke another certificate in contract 0.3.0 (0.4.0 should have one); the
only supported way to take a server's certificate out of use is to remove the server. A removal distributes the list immediately: the internal API of every server and, through `ssl_crl_file`, every
PostgreSQL instance refuse the certificate from then on (a reload is enough).

**Limits of this increment.** One map group (the first server) and one server per shard group: no standby, no
failover, no rebalance, no server groups. The map group's server cannot be removed or restarted away from, and
failover of the manager leader does not exist. A server must be reachable at the id it registers; DNS changes
need the trust rules to be applied again (any membership change does that). A cluster-wide task takes the list of servers when it starts: a server that joins while a
`database.create` or `database.drop` task is running is not part of that task (do not overlap them).

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
**Not done:** `listLedgerVerifications`, `getLedgerVerification` (the contract requires a `taskId` the extension does
not record) and `createInclusionProof` (the proof JSON of the extension carries no leaf hash and no left/right
positions, which the contract requires; both would have to be derived or the contract changed).
The task subject of a verification is the database name, because the verification id exists only after the run.

**Alerts** (kinds of the contract): `serverDown` (the instance is not running; since nothing can be stored while it is
down, the outage is recorded as a resolved alert when the instance is back), `diskSpaceLow` (90% warning, 95% critical),
`memoryPressure` (under 10% available), `replicationLagHigh` (over 60 s). Every 10 s; an alert is raised once per kind
and server, cleared when the reading is fine again, and a value that could not be measured neither raises nor clears.
Not covered because the contract has no alert kind for them: ledger verification problems, pending ledger transactions
too old, and `version()` not containing "KingresDB" (the latter is logged as an error). Listing needs `kingres_readonly`,
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
notes 11 to 21. Tested against `kingres_shard` 0.2 as committed in `c283dbf6bc` (`scripts/install-shard-0.2.sh`).
**Not done:** server groups and the cluster-wide logs, alerts and metrics, the certificate renewal job, rebalance,
failover and restoring preferred primaries, and contract 0.4.0 with what depends on it (inclusion proofs,
verifications list and detail, instance start/stop, leave-cluster, the new alert kinds); every one of them still
answers "not supported yet".

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
leader has: `listTasks`, `getTask`, `listAlerts`, `getClusterSettings`) over `POST /internal/v1/forward` after it has
authenticated the caller, checked CSRF and the required role itself; the leader re-checks the operation, the identity
and the role and writes the audit event, once (`forwardedBy`, `originRequestId`). Not forwarded: sign-in/out, setup,
operations on this server, the query workbench. If the leader is unknown or unreachable the answer is 503 and nothing
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
`scripts/smoke3.sh <kingresd> [base-port]` is the same flow driven by curl and psql (output in the increment's report).
`scripts/install-shard-0.2.sh [commit]` builds `kingres_shard` 0.2 from a commit (default `c283dbf6bc`) into
`~/kw-cp/install`, not from the working tree.

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
13. A way to revoke a server certificate without removing the server, and the revocation list of the cluster, are not
    in the contract.

---

Copyright (c) 2026, RooyeKhat Media Co. Released under the PostgreSQL License.
