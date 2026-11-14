<!-- SPDX-License-Identifier: PostgreSQL -->
# kingresd: the KingresDB control plane

`kingresd` is the control-plane server of KingresDB. It serves the management
REST API that the web panel and `kingres-cli` use
(`kingres/api/openapi.yaml`, conventions in `kingres/api/README.md`) and the
embedded web panel, on one port (default `127.0.0.1:8091`). It runs on every
server of a cluster.

This is the **second increment**: it adds the instance manager, the task engine, the system database,
cluster creation (`init-cluster`) and the operations behind the Databases, Security, Query and Settings
pages that one server can do (see [What is implemented](#what-is-implemented)). Joining servers, rebalance,
failover, the ledger digests, verifications and proofs, alerts, logs, metrics, server groups and cluster
settings updates answer the contract's "not supported yet" problem. Design: `kingres/docs/design/control-plane-2.md`.

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
| `pg-port`, `pg-listen` | `KINGRES_PG_PORT`, `KINGRES_PG_LISTEN` | `5432`, `127.0.0.1` | port and `listen_addresses` of the managed instance |
| `internal-listen` | `KINGRES_INTERNAL_LISTEN` | empty (off) | host:port of the mutual-TLS listener of the internal server-to-server API |
| `metrics-token` | `KINGRES_METRICS_TOKEN` | none | bearer token that may scrape `/metrics` besides stats-role session tokens |
| `server-host` | `KINGRES_SERVER_HOST` | machine host name | host registered in the shard catalog; it is the server id |
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
authenticated operation lists in 0.2.0. **64 of 86 operations are real; 22 answer "not yet".**

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
| Servers | `listServers`, `getServer` | `addServer`, `removeServer`, `updateServer`, `failoverServer` |
| Settings | `getClusterSettings`, `updateClusterSettings` |  |
| Network access | `getNetworkAccess`, `replaceNetworkAccess` |  |
| Setup | `initCluster`, `getNodeState`, `checkSetupToken` | `joinCluster` |
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
mismatches the web panel's end-to-end run found (see "Corrections after the panel's end-to-end run"). **Not done:**
server groups and the cluster-wide logs, alerts and metrics, join, add and remove of servers (D4, D5), the renewal
job, "every primary" operations on every server, certificate trust between PostgreSQL instances (D3), the
three-process integration tests and contract 0.4.0 with what depends on it (inclusion proofs, verifications list and
detail, instance start/stop, leave-cluster, the new alert kinds); every one of them still answers "not supported yet",
and a cluster still has exactly one server. Nothing half-working is switched on: the join and add operations are not
routed, and `kingres_shard` 0.2 (needed by D3) was not committed when this was written.

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
skipped and `go test ./...` runs the unit tests only.

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

---

Copyright (c) 2026, RooyeKhat Media Co. Released under the PostgreSQL License.
