<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# kingresd: the KingresDB control plane

`kingresd` is the control-plane server of KingresDB. It serves the management
REST API that the web panel uses (`kingres-cli` is planned, not built, and will use it too)
(`kingres/api/openapi.yaml`, conventions in `kingres/api/README.md`) and the
embedded web panel, on one port (default `127.0.0.1:8091`). It runs on every
server of a cluster.

This README is the developer reference of the control plane at the first release (REST contract 0.34.0). The sections after "What is implemented" are the record of the increments in which it was built, and keep their wording of that time; where one says "not yet", the table under "What is implemented" and the later sections are current. The second increment added the instance manager, the task engine, the
system database, cluster creation (`init-cluster`) and the operations behind the Databases, Security, Query and
Settings pages; the third lets a second and a third server join (`joinCluster`, `addServer`), removes an empty
server (`removeServer`) and makes the cluster-wide operations run on every server (see
[Running several servers](#running-several-servers)). At that time rebalance, failover, restoring preferred primaries
still answered the contract's "not supported yet" problem (rebalance and failover are built since; the table
under "What is implemented" is current, and no operation answers so any more). The fourth step added server groups, leaving a cluster,
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
  scheduler/              the recurring jobs of the manager leader, the term and its guard (ledger phase 7)
  ledgersign/             the signing keys of the ledger: key files, key tasks, events, trust file, jobs (ledger phase 7)
  ledgerkeys/, ledgerlog/, anchor/  libraries of ledger phase 7 (keys and their state machine; logs, tiles, checkpoints; anchoring); ledgerkeys and
                          ledgerlog are wired by ledgersign, anchor by ledgeranchor (steps 10, 12 and 14)
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
.\scripts\run-integration.ps1 -Install ~/kw-cp19/install   # the same suite on another installation
```

`KINGRES_TEST_KEEP=1` (with `KINGRES_TEST_VERBOSE=1`, which keeps the request lines in the logs) keeps the directories of a run,
the logs of every `kingresd` and every instance, after it ends, for a post-mortem (the next run reaps them; copy what is
needed first). Both reach WSL through `WSLENV`.

`-Install` is a path inside WSL (`~` and `$HOME` are expanded there); it reaches the tests as `KINGRES_INSTALL`, so the
suite runs on whatever major that installation is (18 or 19, `cluster.SupportedMajors`). Without it the default stays
`~/kw-cp/install`. An installation of another major is made as described in `docs/dev/testing.md` (a copy of the
installation with its prefix fixed, then the extensions installed into it from the committed tree).

One source serves both majors; nothing is built per major. What differs is decided at run time from the server:
`internal/pgconfig` reads the parameter list, units, contexts and enumerations from `pg_settings` of the running
instance, so the configuration page shows and validates the parameters of the major that runs; a parameter that
PostgreSQL hides from ordinary users and that nobody has reviewed is refused as superuser-only (PostgreSQL 19 added
`ssl_sni` and `hosts_file`, both refused with their own reason). Tests that depend on the major ask the server
(`pgtest.Major`, from `server_version_num`, through `instance.MajorOfVersionNum`) and assert the behaviour of that major.
`scripts/smoke3.sh` takes the installation from `KINGRES_INSTALL` and the console end-to-end run from `E2E_PG_INSTALL`.

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
| `pg-bin-dir` | `KINGRES_PG_BIN_DIR` | none | directory of `postgres`, `initdb`, `pg_ctl`, `pg_config` (the package sets `/opt/kingres/bin`). Unset: the directory of the kingresd binary, then `PATH`; none found: instance management is off and `initCluster` answers 422. A directory that is not a KingresDB build of a supported PostgreSQL major (18 or 19, pre-releases such as `19rc2` included; all programs of one major) stops the server |
| `ledger-keys-dir` | `KINGRES_LEDGER_KEYS_DIR` | `<data-dir>/ledger/keys` | directory of the private signing keys of the ledger (`signing/<fingerprint>.key`, mode 0600, directory 0700). It may be on another volume; back it up off the cluster, separately from the passphrase file (design 9.2) |
| `ledger-key-passphrase-file` | `KINGRES_LEDGER_KEY_PASSPHRASE_FILE` | none | file (mode 0600, one line) with the passphrase that seals the signing key files (PBKDF2-HMAC-SHA256, AES-256-GCM). Read at the start and whenever a key file is read or written, never from the environment or the command line. Without it the key files are in clear text, like `ca.key`; every server that holds keys needs the same passphrase |
| `ledger-publication-listen` | `KINGRES_LEDGER_PUBLICATION_LISTEN` | empty (off) | `host:port` of the HTTP listener that serves the published ledger (checkpoints, tiles, entry bundles, events, the key ring) to anybody, with no authentication; every server can run one (a follower asks the leader). Empty: no listener |
| `ledger-publication-tls-cert`, `ledger-publication-tls-key` | `KINGRES_LEDGER_PUBLICATION_TLS_CERT`, `KINGRES_LEDGER_PUBLICATION_TLS_KEY` | none | certificate and key files of the publication listener, set together and only with `ledger-publication-listen`; without them it is plain HTTP |
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
| `-version` | | | prints version, contract revision and build commit (`-ldflags "-X main.commit=..."`, else `unknown`) |
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

## Configuration from the console (phase 13, contract 0.8.0)

Design: `kingres/docs/design/config-phase13.md`. Code: `internal/pgconfig` (policy, validation, the agent on every
server, the leader's side), `internal/instance` (`managed.go`, `configfile.go`), migration `0007_config.sql`,
handlers in `internal/httpapi/handlers_config.go`, wiring in `cmd/kingresd/config_wiring.go`.

- **Levels.** A PostgreSQL parameter is set for the cluster, for a server group, for one server, or for a database
  (`ALTER DATABASE ... SET`, applied on the primary instance of every server; standbys replay it). A narrower level
  beats a wider one. The values are stored in `kingres_cp.config_value` (with `config_change` for the history and
  `config_server_state` for what a server reported); the file on a server is a rendering of them.
- **The override file.** `kingres-config.conf` in the data directory of every instance (primary and standbys of a
  server get the same file). `kingres-managed.conf` includes it after the derived defaults (`max_connections`,
  `max_prepared_transactions`, `shared_buffers`, `effective_cache_size`, `maintenance_work_mem`) and before its fixed
  block, so an override beats a derived default and never a managed setting. Never `ALTER SYSTEM`, never an edit of
  `postgresql.conf`. A hand edit of the override file is overwritten by the next apply (the leader notices a file
  that is not what the stored values give, every 30 seconds, and brings the server up to date).
- **Managed and refused parameters.** `instance.ManagedParameters` is the one list that the renderer, the standby
  code and the API share (`TestManagedListIsComplete` fails when they disagree). Refused: what runs a command, loads
  code, names a file or holds a connection string (PostgreSQL's own superuser-only set, found at run time, plus
  `*_command`, `*_library`, `*_libraries`, `*preload*` by name, `fsync`, `full_page_writes`, the developer options).
  PostgreSQL 18 shows `archive_command` and `restore_command` to every database user, so they are refused by name and
  their value is never returned by this API. Cluster-only parameters (`TimeZone`, `lc_*`, `DateStyle`,
  `kingres_shard.default_bucket_count`, ...) cannot be set per server group or server.
- **Warnings of the preview** (contract 0.16.0, `pgconfig/warnings.go`) never make a request invalid: `config_value_unusual`
  for a memory value above ten times or below a tenth of the default of the parameter (exactly ten times and a tenth
  pass; a default of zero never warns), and `config_reserved_connections` when on some server `max_connections` would
  not exceed `superuser_reserved_connections + reserved_connections`. The sum is computed from the values the change
  gives and the running ones (a hand-written rule; `postgres -C` does not check it, PostgreSQL checks it at the start).
- **Validation** is PostgreSQL's: `pg_settings` (type, unit, range, enum), then `set_config` in a rolled-back
  transaction or `postgres -C` for the value, then `pg_file_settings` on the written file before it is reloaded
  (a bad file is replaced by the previous one and never loaded). Nothing is written before every change of a
  request has passed.
- **Applying** is the task `config.apply` (one step per server; a server that is down makes the task wait and say
  which; a server that fails does not stop the others, the task ends `config_incomplete` and
  `GET /config/servers` shows applied, pending restart, failed or waiting per server). A parameter of context
  `postmaster` is pending a restart until the instance restarts; `POST /config/restarts` (task `config.restart`)
  restarts the waiting instances one server at a time, a standby before its primary when a value goes up and after it
  when one goes down (the five settings a hot standby needs at least as high as its primary), and says that
  restarting a primary is downtime for its group (failover does not exist yet).
- **Time.** The console shows each server's clock, operating system time zone, `TimeZone` and `log_timezone`, and the
  difference between the clocks, measured over the internal API (five probes, the smallest round trip, uncertainty
  half the round trip plus 1 ms; the round trip comes from the monotonic clock). The alert `clockDifference` is
  raised by the manager leader when the difference less its uncertainty is above `clock.maxDifferenceMs`
  (cluster setting, default 1000). **`kingresd` does not set the system clock** of a server: that needs the rights
  of the operating system's administrator and is the job of time synchronization (chrony, NTP,
  `systemd-timesyncd`). When clocks differ, fix the synchronization service on that server (`chronyc tracking`,
  `timedatectl timesync-status`), do not set the clock by hand.
- **Tests.** Unit: `internal/pgconfig`, `internal/instance` (`managed_test.go`). With a real PostgreSQL
  (`internal/pgconfig/pg_test.go`): the differential test of the validator, the refused set of the running version,
  the override file with hostile values, the check before the reload. On three servers
  (`internal/clustertest/config_test.go`): cluster, group and server levels, removal, pending restart and the restart
  task, the order of standbys and primaries, refused and invalid values, a server that is down, a server that fails,
  database and cluster time zones, the clock difference, history and revert, audit, roles. The tests that the check of
  the phase against its design found missing are in `config_closing_test.go`: `pg_file_settings` clean on every server, a
  value that PostgreSQL rejects in the file (the file is restored and the text is reported), a database time zone seen
  on a standby, and the warnings of the preview.

## Data transfer: table CSV export and import (phase 14a, contract 0.19.0)

Design: [../docs/design/data-export-import.md](../docs/design/data-export-import.md) (its last section before "Known limits" says where the code
differs from the text). Ten operations, tag `Data transfer`: two tasks (`table.export`, `table.import`) and eight operations on staged files.

**Who and what is checked.** Every operation needs `kingres_admin`, the cluster setting `dataTransfer.enabled` (off by default; the leader reads it, a
follower cannot) and a request that arrived over TLS (the server that received it decides; start `kingresd` with `-tls-cert-file` and `-tls-key-file`).
The data is bound by the PostgreSQL privileges of the signed-in role: the connection of an export or an import is the role's. A staged file belongs to
its creator by **name and OID**; another role sees 404 (also another administrator), only a delete is open to every administrator. The role is checked again
at the start of every step and before the commit (`CurrentRoles`, the OID): a role that signed out, was demoted, expired or was replaced ends the task with
the code `forbidden` and stores nothing.

**Where files live.** `-transfer-dir` (default `<data-dir>/transfer`, mode 0700, marked by `.kingres-transfer`; refused when it is not empty and unmarked, or is or
holds the data directory), the master key `-transfer-key-file` (default `<data-dir>/transfer.key`, never inside the staging directory, in file-level
backups of the data directory), `-transfer-max-duration` (24h; the COPY carries the time left as its `statement_timeout`) and `-max-atomic-bytes` (2 GiB of
decompressed CSV per import). Files are sealed in 64 KiB segments with AES-256-GCM (package `staging`, increment CP2). Limits and the janitor are
in the design note; the janitor runs on the leader every 5 minutes and once before the first transfer, expires files, deletes the files of dropped roles and
stops when the files show an id the system database never issued (a restored database).

**Streaming relay.** `uploadDataChunk` and `downloadDataExport` carry `x-streams`; they leave the 30 s request timeout and the 1 MiB body limit and use idle
deadlines that move with the bytes (package `relay`, `internal-api.md`, "The streaming relay"). A follower relays them to the leader through
`POST /internal/v1/forward-stream` without buffering. Spike S1 moved 5 GiB each way with a heap growth of 3 and 13 MiB.

**Export.** One `COPY (SELECT ...) TO STDOUT` of the table in a read-only `READ COMMITTED` transaction of the role, `row_security = off`, fixed session formats
(`DateStyle`, `IntervalStyle`, `extra_float_digits`, `bytea_output`, `TimeZone`, `lc_monetary`, `client_encoding`), through the compression and the seal; at most
three attempts for a read that a moved bucket or a lost connection interrupted. The statement is built from names that passed `sqlx` and from checked options;
`spreadsheetSafe` (default on) prefixes risky text cells with an apostrophe in SQL (a convenience, not a security feature; it loses the padding of `char(n)` and
the value it changes). The consistency label is `single-snapshot` for a local or reference table and for a distributed table on one group, `per-group` otherwise;
**there is no global snapshot**. A table with row-level security is exported only by its owner or a role with `BYPASSRLS`, and a read that would run a function
of another role needs `acknowledgeOtherRolesCode`.

**Import.** All or nothing, in one step (*Load rows*): `BEGIN`, `COPY ... FROM STDIN`, the count of the server compared with the records the scanner counted, an
insert into `kingres_transfer.import_marker` (a row keyed by a random 128-bit nonce that the task carries, written through the security definer function
`kingres_transfer.mark(nonce, task, rows, tag)`, which inserts only when `tag` is SHA-256 of a per-database secret and of `nonce:task:rows`; only the control plane can compute it, so no
role can forge, pre-empt or hold a marker, and the role needs no grant and can still be dropped; the secret and the table are readable by the service role only; markers older than
30 days are deleted; the schema, tables and function are created and checked by the service role on every use and the import fails closed when the schema exists with another
owner or holds a trigger, rule, policy, other column or other function, so a database owner cannot plant `kingres_transfer`; an operator removes a planted schema), the commit intent written in the detail of the task under the lease, `COMMIT`. No transaction lives between steps. The marker commits with the rows or not at
all, so after any interruption the answer to "did it commit" is read from the database: a run that finds the intent asks the marker; a row `committed` finishes the task
(*Confirm the commit* records the count and the audit event); otherwise the run inserts a tombstone `aborted` for the task (it waits up to 60 seconds for a transaction
that holds the key) after which no transaction of that task can commit, and fails `copy_interrupted` (retryable) with the proof that nothing was stored. `nothing was
stored` is never said without that proof. When the question cannot be asked (the lock wait ran out, or the marker is on the server that was the leader before a
change of leader) the task fails `import_outcome_unknown` and is never retried. A cancel is accepted until the intent is written. The role is refused at the request
and in the step when it has SUPERUSER, REPLICATION or BYPASSRLS or reaches server files, programs or logs (an import runs the triggers and defaults of the table as that
role). The setting `dataTransfer.enabled` is read again by the first step of a task: turning it off ends a task that has not begun to move data; a task that is moving
data is cancelled with `cancelTask`. A transient error of the role check is retryable (`transfer_session_lost`), not a refusal. `done` means the local commit happened; a group whose `COMMIT PREPARED` is still pending
may show its rows a moment later. A file exported with the spreadsheet guard and a header has an apostrophe in front of a header cell that starts like a formula; import it with `header: skip`
(`match` compares the names). The marker table is local to the database of the server that loaded (design: a reference table for the batch import of 14c). A reference table is loaded through a temporary table and
inserts from typed arrays (`unnest($1::int[], ...)`), cap 256 MiB, and refuses array columns. The scanner refuses an unquoted `\.` line (`import_end_marker`;
on PostgreSQL 18 and 19 it is data in CSV, the refusal is the conservative choice) and a record above 16 MiB; a gzip stream is counted against the limit. **Identity
columns of a distributed table draw from the sequence of the node the statement runs on** (design F18): rows loaded without the column from different servers can
repeat values; list the column in the file.

**Errors.** One function, `transfer.SafeError`, builds every error of a transfer from the SQLSTATE, a sentence per state, the names of the schema, table, column and
constraint (the table is the task's own; a bucket partition name is dropped) and the absolute line number; never the message, detail, hint, context or the row, and
never the `where` of the router (it holds the socket directory of a node). No problem, task error, task detail, audit event or log line of a transfer has a value
of the data (tests plant a canary in the failing row of every class).

**Tests.** `internal/relay` (spike S1, `KINGRES_SPIKE_BYTES=5368709120` for the full size), `internal/transfer` (SafeError, scanner, options, record; the record tests
need PostgreSQL), `internal/staging` (CP2), `internal/httpapi` (guards, relay), `internal/internalapi` (the call against a real mutual TLS listener),
`cmd/kingresd` (`TestUpload*`, `TestExport*`, `TestImport*`: real kingresd over HTTPS, `-max-atomic-bytes 4194304`), `internal/clustertest`
(`TestDataTransferThroughAFollower`: three servers, uploads, downloads, import and export through followers, a follower that dies in the middle of an upload).
Run the PostgreSQL ones with `scripts\run-integration.ps1 -Packages ./cmd/kingresd -Run 'TestUpload|TestExport|TestImport'` on both majors.

**Not built in 14a.** Batches, `skipRows`, `onError` (the router refuses `ON_ERROR`; the console never offers it, nor `LOG_VERBOSITY`), a real failover of the manager leader at every step (the tests restart the leader on its own disk and stop a shard group instead).

**Metrics and alert (added to 0.19.0 before its release).** `transferStagingUsedBytes`, `transferStagingFreeBytes`, `transfersRunning`, `transfersStaged` (server metrics of the
server that holds the files) and the alert `transferStagingLow` (warning at 90 percent of the staging quota, critical at 98).

**Fault tests added after the first review round.** `TestImportRecordNearTheLimit` (15.5 MiB through PostgreSQL, 16 MiB and 1 byte refused), `TestImportCancelAfterTheIntent` (409,
the import completes once), `TestImportLeaseLostBeforeTheIntent` (the write of the intent refused: a take-over loads once, no doubled rows), `TestImportCrashAfterCommit` (the lease lost after COMMIT: the marker finishes the task with exactly the rows), `TestImportCommitFaultPoints` (a connection killed after the intent: the marker proves that nothing was stored), the tests of the fixes of the review in `review14_e2e_test.go`, `TestExportUntrustedFunction` (a policy
of another role: `export_untrusted_code`), `TestStagingMetricsAndAlert`, and on three servers `TestDataTransferLeaderRestartMidTransfer` and
`TestDataTransferShardGroupLostMidCopy` (rows are all or none; no prepared transaction is left).

## Database export and archive validation (phase 14b, contract 0.29.0)

Two more operations of the tag `Data transfer` and two tasks, built on the staging store, the encryption at rest, the uploads and the roles of phase 14a (design:
`docs/design/data-export-import.md`, "Database export [14b]" and "Results of the spikes and changes made by the code (phase 14b)"). The revision number is a
placeholder.

- **`exportDatabase`** starts `database.export` (subject: the database). Steps: *Check the request* (what the archive does not carry, the privileges of the role on
  every table, large objects), *Take the structure* (one `REPEATABLE READ READ ONLY` transaction of the service role: the catalog, the fingerprint, and `pg_dump` of the
  build of the instance, schema only, in the snapshot of that transaction), *Normalise the schema* (package `dumpnorm`, then the shape grammar of package `sqlshape`),
  *Write the data* (the streaming zip writer of package `archive`, the data read as the signed-in role by `COPY (SELECT)`), *Check the archive* (the file is read back
  through the validator and the fingerprint is compared: `schema_changed`) and *Seal*. A table task of the database is refused while it runs and the export waits for a
  table task. The archive is a file of the staging store like an export of 14a; `downloadDataExport` serves it as `application/zip`.
- **`validateDatabaseArchive`** starts `archive.validate` (subject: the upload) for an upload of the purpose `databaseArchive`: the archive is read once, checked as the
  importer will check it, and the `ArchiveReport` is stored with the upload and cached by the SHA-256 of the file. It touches no database.
- **Packages.** `internal/archive` (writer, validator, manifest and catalog decoders, version rules, report), `internal/sqlshape` (lexer and shape grammar, validate-only),
  `internal/dumpnorm` (the normaliser of the dump, with the golden files of PostgreSQL 18 and 19), `internal/ops/dbexport*.go` and `archivevalidate.go`.
- **The binary directory.** `pg_dump` is run from the directory of the PostgreSQL programs of the instance (`-pg-bin-dir`, or the one found), never from `PATH`, with fixed
  arguments, an environment of its own and its error output discarded.
- **Tests.** Unit tests of every package, a PostgreSQL test of the facts and the dump (`TestReadFactsAndDumpOfARealDatabase`), cluster tests in `cmd/kingresd`
  (`TestDatabaseExport*`, `TestSpikeS3DumpOfEveryTableKind`) and `internal/clustertest` (`TestDatabaseExportOverThreeServers`); run on PostgreSQL 18 and 19 with
  `scripts/run-integration.ps1`.

## Database import (phase 14c, contract 0.30.0)

One more operation of the tag `Data transfer` and one task (design: `docs/design/data-export-import.md`, "The task database.import", "What may run: the shape grammar (14c)"
and "Results of the spikes and changes made by the code (phase 14c)").

- **`importDatabase`** starts `database.import` (subject: the new database). The archive is hostile input and is read again from its bytes (the SHA-256 of the file against
  `expectedSha256`, the whole validator, the shape grammar over the three SQL files), whatever the state of the upload says; a cached report may only make a request fail early.
  Steps: *Check the archive* (versions, the closed list of extensions, the plan, the ledger acknowledgement, `maxImportBytes`), *Create the private database* (and one step for each
  other server), *Install the extensions*, *Check the functions* (every function that an expression calls is looked up in `pg_proc` of the new database and must be builtin and not
  on the list of the dangerous ones, or belong to a contrib extension of the manifest), *Create the tables* (the statements of `pre-data.sql` as the signed-in role), *Distribute the
  tables* (from the catalog document, while they are empty), *Create the views*, *Load the data* (one transaction per table, the rows counted against the manifest), *Create the
  indexes and constraints*, *Set the sequences* (on every node), *Enable the ledger and the policies* (new chain, policy set again, time-partition policies disabled), *Verify* and
  *Take the final name* (the rename on every server, the owner, the registry).
- **As the signed-in role, one statement per round trip.** `sqlshape.Each` hands out the exact text that the grammar consumed completely; the text is lexed again alone and must read
  the same; it runs through the extended protocol (the server refuses several statements), on a connection with `search_path = pg_catalog`, after the dangerous-role check, which is
  made again at every step. The service role runs only calls that the control plane writes from the catalog document with identifiers that passed `sqlx`, and only while the role
  cannot connect (`CONNECT` is withdrawn and its sessions are ended); a table that the role made is first checked in the catalog (no trigger of its own, no rule, no policy, no
  call of a function that is not allowed in a default, constraint or index) and the schema of an extension is verified by `extguard` before a call into it.
- **A private name.** `kingres_import_<task id>`, made by the service role (a database that exists is a failure) with a marker comment; the failure hook drops a database only if
  this server made it for that task. A failed or cancelled import leaves nothing under the final name.
- **Flags.** `-max-import-bytes` (100 GiB), next to `-max-atomic-bytes`.
- **Packages and files.** `internal/sqlshape/exec.go` (the execution mode of the grammar), `internal/archive/entry.go` and `assemble.go`, `internal/archive/archivetest`
  (archives that lie, for tests), `internal/ops/dbimport*.go`.
- **Tests.** Unit tests of every package, PostgreSQL tests of the function check, the check of a table, the extended protocol, the search path and the foreign keys
  (`dbimport_pg_test.go`), cluster tests in `cmd/kingresd` (`TestDatabaseImport*`: the round trip, 45 hostile archives, hidden text, a dangerous role, the access of the role, cleanup,
  a database that the task did not make, the archive of another major) and `internal/clustertest` (`TestDatabaseImportOverThreeServers`); run on PostgreSQL 18 and 19 with
  `scripts/run-integration.ps1`.

## What is implemented

The server implements revision **0.34.0** of the contract (178 operations, 142
paths). "Real" means the operation works against PostgreSQL or the local node
state. An operation that a server does not implement answers, after authentication, CSRF and role checks,
**HTTP 422 with `code: unsupported_operation`**, which every authenticated operation lists. **All 178 operations are real; none answers so.** The one exception inside an operation is the check `anchors` of `startLedgerVerification`, which answers 422 `unsupported_operation`.

| Tag | Real | Not yet |
|---|---|---|
| Alerts | `listAlerts`, `acknowledgeAlert` |  |
| Audit | `listAuditEvents` |  |
| Auth | `login`, `logout`, `getSession` |  |
| Cluster | `getCluster`, `getClusterSummary` |  |
| Roles | `listClusterRoles`, `listRoles`, `createRole`, `getRole`, `deleteRole`, `updateRole`, `setRoleClusterRoles`, `setRoleMemberships` |  |
| Databases | `getDatabaseCreateOptions`, `listDatabases`, `createDatabase`, `getDatabase`, `dropDatabase`, `updateDatabase`, `getApproximateAggregates`, `configureApproximateAggregates` |  |
| Schemas | `listSchemas`, `getSchema`, `distributeSchema`, `undistributeSchema`, `moveSchema` |  |
| Shards | `getBucketDistribution`, `listColocationGroups`, `listBuckets`, `previewBucketSplit`, `splitBucket` |  |
| Ledger | `getLedgerStatus`, `listLedgerBlocks`, `listLedgerDigests`, `generateLedgerDigest`, `startLedgerVerification`, `listLedgerStatus`, `createInclusionProof`, `listLedgerVerifications`, `getLedgerVerification`, `getLedgerSettings`, `updateLedgerSettings`, `getLedgerSigning`, `createLedgerSigningKey`, `retireLedgerSigningKey`, `revokeLedgerSigningKey`, `getLedgerTrustFile`, `getLedgerPublication`, `getLedgerAggregate`, `listLedgerAggregates`, `listLedgerAnchorTargets`, `createLedgerAnchorTarget`, `getLedgerAnchorTarget`, `updateLedgerAnchorTarget`, `deleteLedgerAnchorTarget`, `testLedgerAnchorTarget`, `listLedgerAnchors`, `anchorLedgerNow`, `retryLedgerAnchor`, `downloadLedgerAnchor`, `listLedgerClientKeys`, `registerLedgerClientKey`, `revokeLedgerClientKey`, `setTableSignaturePolicy`, `upgradeLedgerExtension`, `createLedgerBundle`, `getLedgerBundle`, `acknowledgeLedgerDivergence` | |
| Tables | `getTable`, `distributeTable`, `enableTableLedger`, `listTables`, `previewTableChange`, `listRelatedTables`, `undistributeTable`, `alterTableDistribution`, `listTablePartitions`, `createTablePartition`, `attachTablePartition`, `detachTablePartition`, `dropTablePartition`, `setTableStorage`, `setPartitionStorage`, `getPartitionPolicy`, `putPartitionPolicy`, `deletePartitionPolicy`, `runPartitionPolicy`, `listPartitionPolicyRuns`, `getPartitionPolicyRun`, `planPartitionPolicy` |  |
| Grants | `listGrants`, `createGrant`, `revokeGrants`, `applyGrantTemplate` |  |
| Health | `getHealth`, `getReadiness` |  |
| Logs | `listLogs` |  |
| Metrics | `queryMetrics` |  |
| Query | `executeQuery`, `cancelStatement` |  |
| Rebalance | `getRebalanceStatus`, `startRebalance`, `previewRebalancePlan`, `listRebalanceReports`, `getRebalanceReport`, `stopRebalance` |  |
| Replication | `getReplicationTopology`, `getFailoverStatus`, `listFailoverReports`, `getFailoverReport`, `restorePreferredPrimaries`, `abandonMapGroupBootstrap` | |
| Server groups | `listServerGroups`, `createServerGroup`, `deleteServerGroup` |  |
| Servers | `listServers`, `getServer`, `addServer`, `removeServer`, `updateServer`, `renewServerCertificates`, `revokeServerCertificates`, `failoverServer`, `drainServer`, `previewServerDrain`, `getCaEscrow`, `setCaEscrow`, `rotateCaEscrow` |  |
| Configuration | `listConfigParameters`, `getConfigParameter`, `previewConfigChange`, `applyConfigChange`, `listConfigChanges`, `revertConfigChange`, `listConfigServers`, `getConfigSummary`, `getClusterTime`, `listTimeZones`, `restartPendingInstances` |  |
| Settings | `getClusterSettings`, `updateClusterSettings`, `resetAutoFailoverCounter` |  |
| Network access | `getNetworkAccess`, `replaceNetworkAccess` |  |
| Data transfer | `exportTable`, `importTable`, `createDataUpload`, `getDataUpload`, `uploadDataChunk`, `deleteDataUpload`, `listDataExports`, `getDataExport`, `downloadDataExport`, `deleteDataExport`, `exportDatabase`, `validateDatabaseArchive`, `importDatabase` |  |
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
instead of inventing zeros. In the integration test of the REST layer (one instance, no manager and no agent) these are `null`, because no
catalog or agent provides them there. The list is enforced by
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
- `NodeStateInfo.setupTask` until a setup request was accepted (the integration
  test sends none).

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
  ledger/keys/signing/     the private signing keys of the ledger, <fingerprint>.key, mode 0600 (setting ledger-keys-dir moves it); only on the
                           manager leader and on the servers that run an instance of shard group 0
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

### Waiting for the instance and for the leader (flaky-run fixes)

Four failures that showed only in a complete run on a loaded machine led to these rules.

- **A request waits for the instance before it fails.** Sign-in, the re-check of a session's role and the join check
  go through `auth.Retrying`: an unreachable instance (`ErrUnavailable`) is asked again with a growing pause for up
  to 10 s (`auth.DefaultUnavailableWait`), shorter than the request timeout. An answer (`ErrInvalidCredentials`) is
  never retried, and after the wait the request is still refused with 503 and `Retry-After`: a role that cannot be
  checked fails closed, only the wait is new. The join call of the internal API (reachable without a client certificate) lets at most 8 callers wait at
  the same time (`auth.RetryingLimited`, ST-10); the others get 503 at once. The internal API asks the catalog about the caller's membership for
  up to 5 s before it answers 503 `unavailable`, so a follower's certificate renewal does not see a leader whose
  own instance restarts for a moment as "the manager leader answered 503".
- **No sign-in against an instance that is about to be stopped.** `lease.Manager.Confirming` is true while an
  enforced group this server runs the primary of has no valid lease and the manager has not yet judged it lost: in
  particular before the first round after a start of kingresd, when the watchdog still counts down the lease of the
  previous run. `auth.Gated` makes sign-in and role checks answer `ErrUnavailable` meanwhile, and `Retrying` waits
  for the round. It is **not** true while a renewal fails and the lease still holds (a follower that lost its
  leader for a few seconds goes on serving: the instance runs until the lease ends), and not after the lease ended:
  the manager then fences the instance and the requests meet an instance that is not there, in the usual way.
  Tests: `TestConfirmingIsNotTrueWhileAFailedRenewalLeavesTheLeaseValid` (both directions, unit),
  `TestConfirmingUntilTheFirstRoundAfterAStartIsDone`, `TestNoSignInAgainstAnInstanceTheLeaseIsAboutToStop` (fault
  `grantDelayMs`).
- **A server that is behind a configuration not tried yet is brought up to date at once.** The leader's loop that brings servers up to date
  waits a minute between two attempts for one server; the wait now applies to a repeat of an attempt at the same configuration (by its hash)
  that was not seen to work: a server that is seen up to date is forgotten, so one whose group changed a minute after an earlier catch-up is not left behind. Found by
  running `TestConfigServerGroupAndServerLevels` several times against one cluster. A server that keeps failing is tried again after one minute, then two,
  four and eight, at most ten minutes (`retryWait`, review ST-08); the task list is not pruned (a decision, see "Closing review of phase 4", P4D-04).
- **One start or stop of an instance at a time.** The restart of a standby failed after 60 s with `pg_ctl stop: server does not shut
  down` although the instance had shut down and was running again: `pg_ctl stop -w` waits until the pid file is gone, and the agent
  of the standbys had started the standby again in the moment it was down, which put a new pid file there. That hang is what made
  the call outlast the write timeout below (the step then failed with the TLS error). `instance.Manager` now serializes `Start`,
  `Stop` and `Restart` of one instance (by its data directory); the shutdown paths that must not wait (`StopImmediate`,
  `StopForShutdown`) do not take the lock. Test: `TestAStartDuringAStopDoesNotMakeTheStopFail` (a fake `pg_ctl`; it fails
  with the old code with the very message above).
- **A long call is answered, not cut.** The internal listener had a write timeout of 60 s counted from the request. A call
  that takes longer (a restart of an instance under load, a statement that runs for minutes) was ended under the
  caller, and a TLS connection that is closed after a failed write has moved its record counter on: the caller saw
  `local error: tls: bad record MAC` instead of a timeout, and a task step failed on a healthy server. The listener
  (`internalapi.NewHTTPServer`) has no write timeout; the write of the answer is bounded (30 s) from its first byte.
  Test: `TestACallLongerThanTheResponseTimeoutIsAnswered` (it also shows the old configuration failing).
- **A step repeats a call that a broken connection failed.** `peerclient.Client.Do` repeats a call, up to three tries
  with a short pause and the connections of that server dropped, when the connection failed under it and the
  call may be repeated: every GET, and every call a task step makes (the engine runs a step again from its start after
  any interruption, so the calls of a step already have to give the same result when repeated, and the callee
  refuses a repeat that comes after a newer run through the fence). A call outside a task that changes something
  (a forwarded request of a person, a certificate renewal) is never repeated: it may have been done once. Not
  repeated either: a server that is no member, a revoked certificate, a partition of a test. Test:
  `TestAConnectionThatFailsUnderACallIsRetriedWhereTheCallMayBeRepeated`.
- **A reset has no window with a handshake failure, and the bootstrap certificate is made once.** `ResetMember` deletes the
  certificates and then records the state `uninitialized`; between the two the internal listener presented nothing
  (`tls: internal error`, which the leader took for a server that cannot be reached). While `Service.Resetting()` it
  presents the bootstrap certificate, so the leader's call meets "not a member of this cluster" (`internalCertificate`,
  test `TestInternalCertificateDuringAReset`). `pki.EnsureBootstrapCert` is serialized and checks that the two files make a
  pair: handshakes that arrive at once while the files do not exist yet each made a key pair, and the files could end up
  as the key of one and the certificate of another, which nothing repaired (`TestBootstrapCertIsCreatedOnceUnderConcurrentCalls`).
- **A joining server is a member only when its own connection works.** `FinishMember` prepares the system database
  and publishes the service connection before the node state becomes `clustered`; the state used to come first, so
  a client that signed in the moment the state changed hit the settings of the instance of an earlier membership
  (503 "The role of this session cannot be checked right now"). Test:
  `TestServerThatJoinedAnswersRoleChecksAsSoonAsItIsAMember` (fault `finishDelayMs`).
- **The peer directory is merged, never replaced, by the task that finishes an add.** `distributePeers` used to
  replace the leader's copy with a list read earlier, which dropped a server that another `server.add` task had just
  introduced: the later task then failed with "no internal address is known" or waited ten minutes at "Wait for the
  new server to hold its certificate". The leader's copy now keeps entries it was not told to remove, the add task
  puts the address from its own input back when it is missing, and the waiting step shows what it waits for in its
  detail. Test: `TestServerIntroducedWhileAnotherAddFinishesIsNotLostFromTheDirectory` (fault `directoryDelayMs`).

The new fields of the fault file (`directoryDelayMs`, `grantDelayMs`, `finishDelayMs`) exist for these tests only.

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

**Revoking a certificate.** The leader (the server that holds the CA) keeps the revoked serial numbers in `<data-dir>/pki/revoked.json`
(serial numbers in hex, with the time); every server keeps a copy for its internal API in `revoked-serials.json`, and the list in the format PostgreSQL reads in `revoked.crl`. Removing a server revokes every certificate that was issued to it, and
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

Limit: an alert is kept by the server that raised it, so after a change of the manager leader (by the failover of the map group, increment 5) the
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
  A step that can read its own effect has a `Recover` (`task.Step.Recover`): it is asked first, `true` records the step `done` without
  running it, `false` (the effect is provably not there) runs it, and an error is `step_interrupted` as before. A step that cannot tell must not have one.
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
Only the two steps of the distributed schemas that call the extension once (`Make the schema distributed`, `Make the schema local`) are marked `NonIdempotent`, with a `Recover` (see Distributed schemas); every other step checks what exists before it creates.

## Not done in this increment, and the manager's state on a leader change

**Not done at the first release:** the rotation of the CA itself, and a change of the manager leader without the CA key unless the escrow is on (the three bullets
below; a leader change by the failover of the map group is built, and the key of the CA can be escrowed). Certificate renewal and revocation, the dedicated problem for a leader without the CA key, cluster-wide
logs, alerts and metrics and the four alert kinds that nothing raised are done (see "Fifth increment"). Server groups, `updateServer`, inclusion proofs, ledger verification records, instance start and
stop, leave-cluster and local settings are done (see their sections).

**What has to move with the manager (for the high-availability increment).** The certificate authority key
(`<data-dir>/pki/ca.key`), the revocation list (`revoked.json`), the task engine (rows of `kingres_system` on the
leader's instance), the directory of internal addresses (`peers.json`, on every server), the audit file
(`audit.jsonl`, per server) and the session store (memory, per server) belong to the first server today. A leader
change has to move the CA key and the revocation list (encrypted, to the standby leader) and the
`kingres_cp` schema (it lives in the map group and follows it); followers already re-read the leader from the catalog
every 2 s. The failover of the map group (increment 5, "The map group" below) changes the leader. With the escrow of the CA key switched on ("Escrow of the key of the certificate authority" below) the new leader opens its sealed copy and goes on; without it the new leader raises the alert `certificateAuthorityUnavailable` until the operator restores `pki/ca.key` and `pki/revoked.json` from the old leader's backup. The limitation therefore stands: **if the manager leader is not the server that holds the CA, operations that
need the CA (adding a server, removing one, certificate operations) cannot work**; the cluster keeps running on its
existing certificates until they expire (one year). See `docs/design/ha-phase5.md`.

**Restoring the system database.** After a restore of `kingres_system` from a backup, restart every `kingresd` of the cluster. The manager leader raises the term of the ledger (`kingres_cp.ledger_cluster.term`) when it takes over, and every write of the ledger's key set checks that the stored term is the one the leader holds in memory; a restored database holds an older term, so those writes fail with `term_over` until the leader has taken over again, which a restart does (see "The scheduler and the term").

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
   start-up requires `postgres`, `initdb`, `pg_ctl` (version 18 or 19, one major for all three) and `pg_config`, and that `kingres_shard.so` and
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
`scripts/install-extensions.sh [commit]` builds `kingres_shard` and `kingres_ledger` from a commit (default: the repository HEAD) into `~/kw-cp/install`, or into the installation named by `KINGRES_INSTALL` (the one of the other PostgreSQL major, for example), not from the working tree, and records the commit in `extensions-commit.txt` of that installation. Run it again after the extensions change: the suite tests the control plane against what is installed there, not against the repository.

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
| CP2-01 / CP-07 forwarded identity by name | fixed | Per-request SCRAM-style proof of a session of the named user, checked against the user's verifier by the leader only (`internal/fwdproof`, `internal/scram`, `auth/proofkey.go`); cluster-wide reads carry an assertion signed by the leader (`POST /gather-authorize`) instead of a proof, so no other member receives a value from which a ClientKey can be derived, except the candidate of a failover of the map group, which receives the proof of the operator's request because the leader is gone (F4-05, ADR 0026); pending servers authorize nothing; `Proxy.Allow` runs after the guard. See `internal-api.md`, "Forwarded identities need a proof". | `fwdproof` tests, `internalapi` `TestPendingServerAuthorizesNothing`, `TestUnprovenIdentityIsRefusedBeforeTheBackend`, clustertest `TestForwardedIdentityNeedsTheProofOfASession` (real three-server cluster; with the check switched off the same forward without a proof answered 200) |
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

1. **Forwarded identities carry a SCRAM-style proof** checked against the user's verifier, sent to the leader only (the one exception is the request of an operator for the failover of the map group, sent to the candidate that becomes the leader, ADR 0026; a receiver of a proof recovers the ClientKey, as a SCRAM server does; a gather therefore carries an assertion signed by the leader, not a proof). A signed assertion of the
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

### The fencing rule of the task engine

**A holder that lost the lease of a task does nothing more in the name of that task**: no write to the system
database, no call to a node that starts or changes work, no hook, no alert. The rule belongs to the engine, not to
a task kind, so that failover can rely on it. How it is kept:

- **Records.** Every state change of a task (`SetStep`, `Renew`, `Finish`, `Pause`, `SetDetail`) names holder and
  epoch and is refused with `ErrLeaseLost` when either differs; marking a step `running` also needs a lease that
  has not expired on the database's clock.
- **Hooks.** `Definition.OnFail` runs only after the engine itself recorded the task as failed or cancelled under
  its lease (`Finish` succeeded). A run that was refused its lease, or whose callee answered `fenced`
  (`fence.ErrFenced`), returns `ErrLeaseLost` and records nothing and runs no hook: "the lease is lost" is not a
  failure of the task, whatever error the step returned because of it (a connection error, a timeout). The hook
  sees the task as recorded (state `failed`, the error).
- **Regaining a lease (RB-02).** A lease that ran out while nobody took the task over leaves the task `running`
  with an expired lease. The leader's next poll claims it again (the epoch grows), settles what the interrupted
  step left behind and runs the step again from its start, as after a restart. If another server claimed it
  first, that server owns it and the old holder stands down.
- **Calls to servers** carry the epoch (`X-Kingres-Fence`) and the callee refuses a lower one than it has seen
  (see "Carry-overs"). A callee that has not yet seen the newer run cannot refuse; see "Limits that remain".
- **Calls to a node's instance that start work** (the rebalance's `move_bucket`) are guarded by a lock on the
  task row held in the system database while the call is issued; see `internal/rebalance` and the section on
  rebalancing.
- **Cancel** of a paused task is allowed (`cancellable` is true for pending, running and paused tasks); it ends the
  task as `failed` with the code `cancelled`, and the hook sees that record.
- **Standing down is logged, and a fence that never clears is bounded (RBR-07).** A run that is refused its lease or
  its fence logs `task run stands down` with the task, kind, step, holder, epoch and the reason. A callee that refuses the
  fence while the store still lets this server hold the lease (its mark is above any epoch reached, as after a restore
  of the system database that makes task ids and epochs repeat) used to make the task loop for ever, a lease
  period per pass, with nothing to see. After `task.MaxFencedRuns` (5) such runs in a row the engine records the task as failed under its
  own lease, with the code `fenced`, and the hook of its kind runs (the rebalance raises `taskFailed` there). A holder
  that does not hold the lease any more cannot record that, and does nothing, as before. Test:
  `TestACalleeThatAlwaysRefusesTheFenceFailsTheTaskAfterABound`.

Tests: `internal/task/fencing_test.go` (`TestSupersededHolderRunsNoHookWhateverTheStepError`,
`TestExpiredUnclaimedLeaseIsClaimedAgainNotFailed`, `TestHookSeesTheRecordedFailure`,
`TestCancelPausedTaskIsCancellableAndTheHookSeesTheRecord`, `TestCancelRequestedWhileRunningWinsOverAPause`) on the
memory store, and `TestPGSupersededHolderRunsNoHook` on a real system database.

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
  follower for four seconds, starting when its group lease has just been renewed, and every read answers 200. The
  follower runs the primary of a group that has a standby, and without a majority that instance runs only as long as
  the lease it holds (5 to 7 s with the default lease settings), so "short" is shorter than that, whatever the bound
  of 30 s says about authorizing sessions; a partition that lasts longer fences the primary of the isolated server and
  its reads fail with 503 until the lease is back, which is the lease doing its job. The default bound of 30 s is three
  times the default failure timeout.
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
  it writes the **fence marker** of the group (`<data-dir>/lease/fenced.<group>`), then stops the instance with `pg_ctl stop -m immediate`
  (no checkpoint, no waiting for clients), and audits `leaseLost` and `leaseFenced`. While the marker exists the instance
  manager's start gate refuses to start the instance (`instance.ErrLeaseRequired`: the instance start operations fail,
  the start at kingresd's own start is skipped with a warning; decision 7 adds the wait marker for a cold start). The next successful round removes the marker, starts
  the instance and audits `leaseAcquired`. The marker is on disk, so a restart of kingresd or of the machine does not
  start the primary without a lease either.
- **Rounds of several groups.** A server that runs the primaries of several groups renews them side by side (at most eight rounds at
  once, one round lock per group; a group whose previous round still runs is skipped until the next tick), and the catalog
  topology is read by a loop of its own into a cache, never inside a round. Rounds that followed each other would make the
  renewal period of every group the sum of all round times, so that the leases lapse under load although each round succeeds.
- **Enforcement rule (decided).** Self-fencing is enforced only for a group that has at least one standby a failover may use (the standby
  manager answers `Standbys().HasStandby(group)` from local knowledge: a `ready` entry of the list, or one in a rebuild; a
  standby that is still being created does not count, so a primary is not fenced while the first copy of it is made, when
  nothing could be promoted anyway). The leader makes a record `ready` only after the primary's server took a list with the entry
  `ready`, so the fence is armed before a failover can choose the standby. For a group without a standby nothing could be
  promoted, so stopping the only copy would cost availability and buy nothing: the lease is still acquired, renewed and
  reported, and its loss raises the alert `leaseLost` (critical, subject `server:<id>|group:<n>`) and the audit event
  `leaseLost`, without stopping the instance for a lapsed lease (only a `kingresd` that stopped running, through the watchdog file that is refreshed by a loop of its own, F2-07). A one-server cluster has one member and a majority of one: it grants itself
  its lease.
- **Watchdog.** `kingres-watchdog` (standard library only, about 90 lines) covers a dead or hung `kingresd` next to a
  live primary. For an enforced group kingresd starts it after the first successful round, detached in its own session
  (a signal to kingresd's process group does not reach it; it takes an exclusive lock so one runs per instance). After every
  renewal kingresd replaces `<data-dir>/instances/<id>/lease.json` of the instance (one per primary instance; `{"seq": N, "validMs": V}`, or `{"off": true}` for an instance that is not a primary now): `seq` changes with every
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
is paused the watchdog does its job. A step of the system clock does not matter for a lease, the watchdog or a session check because no decision of theirs reads it
(`clock.Clock.Mono`); the freeze file, the task lease in the system database and the proof of a forwarded request do read wall clocks (see the review F2-15); the tests step the wall clock of a server through the fault hook and see nothing change.

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
| `standby-replace-after` | `30m` | how long the record of an old primary whose server does not answer counts as a planned standby before a replacement is made on another server |
| `slot-retention-warn-bytes` | `1073741824` | WAL a standby's slot may retain before `replicationLagHigh` is raised |
| `slot-retention-limit-bytes` | `4294967296` | WAL a standby's slot may retain before it is dropped and the standby rebuilt |
| `standby-port-base` | `pg-port + 1000` | first port tried for a new standby instance (the first free one from there is used and recorded) |
| `standby-port-end` | `standby-port-base + 499` | last port tried for a new standby instance; with the base it gives a server a range of its own (a one-machine cluster, a test run) |

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
   `pg-port + 1`, which on a one-machine cluster is the port the next server will bind), recorded in the standby record. The port is chosen after the base backup, right before the first start (not before the copy, which can take minutes), within `standby-port-base` to `standby-port-end`. If that first start fails because the port could not be bound (the startup log says "Address already in use" or "could not bind"), one other free port is chosen and recorded, in the managed file, the settings, the marker and the standby record together, and the start is tried once more. A standby that has run keeps its port; only an instance that has never run (the file `kingres-never-started` in its directory) may change it.
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
| `allow-no-watchdog` | `KINGRES_ALLOW_NO_WATCHDOG` | `false` | tests and development only: lets `kingresd` start without the lease watchdog program `kingres-watchdog`; without the setting a server that runs the lease protocol refuses to start when the program is in neither `pg-bin-dir` nor the directory of `kingresd` |
| `admin-keys-max-age` | `KINGRES_ADMIN_KEYS_MAX_AGE` | `72h` | how old the cache of the administrators' keys (`failover/admin-keys.json`) may be before the declaration that a server is lost refuses it; it must be positive, there is no unbounded mode |

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
3. **Self-fencing is enforced only for a group that has a ready standby (or one in a rebuild)** (decided by the coordinator); a group without one only raises `leaseLost`. A one-server cluster grants itself.
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

- **Catalog registration of standbys:** done in increment 0 of part 2 (see "Failover groundwork" at the end of this file).
- **Promotion:** set `Agent.Frozen` during a failover decision; obtain the lease through `POST /lease/grant` (use `dryRun` to wait for the old lease); make `Agent.maintainPrimary/Status/Create`, `catalogTopology` and `localInstance` in `ha_leases.go` and the watchdog files use the instance id of the group's primary instead of instance 1; repoint the other standbys (`primary_conninfo`); rebuild the old primary with `POST /standby/rebuild` and `diverged: true`; remove `lease.FencedMarker` when a server becomes a standby.
- **Automatic failover** reads `ha.Liveness.State/Silence` (count `autoFailover.timeoutSeconds` from the dead transition) and `Leases`.
- The 60-second leader memory (decision 8) must be revisited with leader moves.

---

Copyright (c) 2026, RooyeKhat Media Co. Released under the GNU Affero General Public License, version 3 (AGPL-3.0-only).

## Rebalancing (contract 0.9.0, first increment)

`internal/rebalance` runs a rebalance as one task of kind `rebalance` (subject `cluster`/`cluster`: one at a time, a stopped one
included), on the manager leader. Design: `kingres/docs/design/shard-phase10.md`, section 20 says where this increment departs from it.

- **Steps.** `Check that the cluster can rebalance`, `Compute the plan`, `Move buckets`, `Write the report`. The third is a loop that
  plans again with `kingres.rebalance_plan()` before every move and makes the first move of the plan that is not backing off. One move runs
  at a time, on a connection of its own (`SELECT kingres.move_bucket(...)` on the map group primary, which is where the leader runs).
- **State** is in `kingres_cp.rebalance_run` and `rebalance_move` (migration 0008); every write of a run is one transaction that proves the
  task's lease and epoch, so a leader that lost the lease can neither record nor start a move. The report of the contract is read from
  these tables; its id is the task id.
- **Stop.** `stopRebalance` sets a flag. The loop asks the extension to cancel the move in flight (`cancel_bucket_move`: before the switch
  it undoes the move; a switching move cannot be cancelled and is asked again until it ended, so the switch is never interrupted), starts
  no other, records the run as `stopped` and returns `task.Pause`: the task is `paused` and `startRebalance` goes on with the same task.
  A stopped rebalance does not block `updateServer` and `removeServer`.
- **Restart and change of leader.** The new leader's engine claims the task under a new epoch and re-enters `Move buckets`; a row left
  `running` is settled through the extension (`resume_bucket_moves()`, waiting while the move's session is alive), never by the control
  plane's own SQL.
- **Retry.** A failed move is tried again after 30 s times its failures (`-rebalance-retry-backoff`), up to three tries; a bucket given up
  ends the run `failed` (the report is written first, the alert `taskFailed` carries the task). Moves skipped by a condition (lease of a
  group lost, a required synchronous standby not in sync, target disk low when reported) are in the report and are not failures.
- **Conditions** before a start and before every move are listed in the design section 20; a refused start is a 409 with its own code.
- **Standbys.** A move changes who owns data, not where standbys are. The slot and the subscription of a move are on the source and the
  target primaries; a group with a synchronous standby that is not in sync is skipped, because the commits of the copy would wait; a
  failover during a move fails and undoes it (the slot is not on the standby). `kingresd` writes `wal_level = logical` into the instances it
  creates; an instance created before needs a restart, and the check says so.
- **Not verified on real servers:** the free-disk check (no server reports its disk yet), the lease check for groups whose primary runs on
  another server (the leader knows only its own leases), a rebalance during a failover (failover is not implemented), and the leader
  change in the middle of a move (the tests restart the leader's `kingresd`; they cannot move the map group).

### Rebalancing after the review (contract 0.9.0)

The review file `kingres/docs/reviews/2026-11-16-kingresd-rebalance.md` found 14 things; what each fix is and where it is proved:

- **Calls to nodes (RB-03).** A lease proved in a transaction that ends before `move_bucket` is called proves nothing about the call, and
  the extension does not take the fencing epoch. So `PG.RunMove` (`internal/rebalance/guard.go`) keeps the proof alive across the call:
  it opens a session on the system database that takes the shared advisory lock of the task (`task.GuardKey`) and then checks holder,
  epoch, state and expiry, checks that this session is still alive, and only then issues `move_bucket` on a second connection. While the
  guard is held another server **cannot claim the task** (the claim tries the exclusive advisory lock and skips the task), and everything else
  works: the holder's own renewals and records, and also a schema change of the task table or a delete of the row, which the lock on the row
  that the guard first took (`FOR KEY SHARE`) kept waiting (RBR-04). The guard is released as soon as the extension has recorded the move in
  `kingres.bucket_move`, or the call returned, or the call was withdrawn (see "Rebalancing after the second review"). It is not held for
  the minutes a copy takes: a frozen holder would keep the lock until the server drops its session (`idle_session_timeout`
  of 45 s is set on it), and the task could not fail over meanwhile.
  **The window that remains** is a process that is frozen exactly between the liveness check of the guard and the write of the call on the
  socket for longer than the idle timeout: its call then reaches the extension after the guard is gone. It is harmless for the data:
  the extension refuses a second move of the same bucket ("already being moved"), so a move the new holder started, or that exists in the
  extension's rows, cannot be doubled; what it does not cover is the interleaving where the late call arrives while **no** move of that
  bucket exists any more (the new holder settled the old row as "not recorded", and has not started its own yet): the late move then runs
  unrecorded for the run until the next `Recover`/plan sees the bucket on its target or the move's rows in the extension. The end state is
  correct (the bucket is on the target, check_cluster is clean); the report may miss that move.
- **Records and hooks (RB-01, RB-02).** See "The fencing rule of the task engine".
- **A lost connection (RB-04).** An error of `move_bucket` that PostgreSQL raised for the statement at severity ERROR ends the move
  (`MoveError`). Any other error (EOF, reset, a FATAL termination of the backend) says nothing about the move: the loop asks the extension
  through `Recover` (`resume_bucket_moves`, waiting while the move's session is alive) before it records anything or starts another
  move. Proved on real instances by `TestRebalanceUnderFaults`.
- **Transient errors (RB-05).** `Databases`, `Check` and `Plan` retry an error that is not a refusal (`*problem.Problem`) four times with a
  growing wait; if it lasts, the run is stopped as a safety stop (outcome `stopped`, `stoppedBy` `safetyRule`, a fixed text; the text of the
  error goes to the log, RB-09), never a failed task.
- **Bound (RB-06).** A part of a run stops (safety stop, with the numbers in the report's `error`) when it made more than
  `3 x the first plan + 10` moves (done, failed or retried): something else keeps changing the placement.
- **Start again (RB-07, RB-11).** A start of a stopped run is a new decision: the run begins a new *round* (`rebalance_run.round`,
  `rebalance_move.round`, migration 0010); attempts, exhaustion and skips count within a round; the rows of earlier rounds stay in the
  report; the options of the request replace the old ones (none selected means all databases). The task is resumed and the run changed in
  one transaction.
- **Cancel (RB-08).** `cancellable` is true for a paused task. Cancelling a stopped rebalance ends the run with outcome `cancelled`
  (stopped by `operator`; the audit log of `cancelTask` names the person), its moves in flight as cancelled, and raises no `taskFailed`
  alert. Cancelling a running rebalance stops the loop between moves like a stop, and the engine then ends the task as cancelled.
- **Exclusion (RB-10).** Migration 0009 refuses, in the database and under one advisory lock (`kingres_cp.lock_operations()`), to create
  a rebalance while a `server.add`, `server.remove`, `server.leave` or `server.failover` task is unfinished, to create such a task while a
  rebalance is pending or running, and to resume a stopped rebalance in that case (SQLSTATE `KX001`/`KX002`, `task.ExcludedError`, 409 with
  the contract's codes). `updateServer` takes the same lock for its check of "no rebalance is running" and the change that follows.
  Not covered by this rule, because the contract does not declare them exclusive: server groups, certificate renewals, standby
  reconciliation and the other task kinds.
- **Open (of the first review).** The system-database writes of the steps of `server.add`, `server.remove`, `server.leave`, `cert.*` and `cluster.init`/`join`
  are protected by the engine's cancellation of the step context when the lease is lost, and their peer calls by the fence header, but are
  not themselves conditional on the lease the way the rebalance's are; the detection delay (at most a third of the lease) is the window.
  The calls of database creation, configuration and standby steps to the local instance carry no guard. `cleanup_moved_buckets(true)` in
  `remove_node` is cluster-wide (RB-14 c); a connection per call and the full plan before every move (RB-14 a, b) are unchanged.

### Rebalancing after the second review (RBR-01 to RBR-07, and what a complete run showed)

The review `kingres/docs/reviews/2026-11-16-kingresd-rebalance-fixes.md` found seven things in the fix round; a manager whose task
another holder took over, in the complete runs, showed what the first of them hides. What each rule is, and where it is proved:

- **A call that has a record is not cancelled (the failures of "a manager whose task another holder took over").** The loop used to
  cancel the call of `move_bucket` when its context ended (a lost lease, a stop, a shutdown). A cancel that arrives while the move is
  switching ends the session in the middle of the two-phase commit of the switch: the move stays in `switching` ("waiting for the write
  lock of the bucket", writes to that bucket wait) until the recovery of the extension undoes it after `2 x node_timeout`, or it leaves
  prepared transactions that make `check_cluster` report `catalog_differs` and `partition_misplaced`, which stops the run of the next
  holder as a safety stop. Now the call has a context of its own that only the end of the run *before the record* can cancel. From the
  record on the move is the extension's: a holder that lost the task stops watching and writes nothing, the call runs to its end, and the
  holder that follows finds the live move by its record and waits for it (`resume_bucket_moves` never touches a live session).
  Tests: `TestRebalanceTakeoverInTheSwitch` and `TestRebalanceTakeoverAtTheCatchUp` (the move is held at a fault point of the extension,
  the task is taken over, the first holder stands down, the move is released), `TestRebalanceUnderFaults`.
- **RBR-01, a slow record is not an attempt.** The guard is held until the record exists, but a call that waits for the cluster lock of the
  extension (a long `add_node`, a distribution of a large table) has no record for as long as it waits. After `GuardHold` (30 s) the loop asks
  the database what the backend of the call is doing (`pg_stat_activity`): a call that is active, a lock wait included, is waited for, up to
  `MaxCallWait` (5 min), with a statement on the session of the guard every 5 s so that the idle timeout of the server does not end it. A call
  that is not running any more, or has waited the 5 min, is withdrawn: `NotStartedError`, and the loop takes the try back (`Rows.UndoStart`: no
  row for a first try, the earlier attempts for a later one), so that it is **not an attempt at the bucket**. Three in a row
  (`notStartedTries`) stop the run as a safety stop with a text of its own; they never give a bucket up. The same holds when the guard or a
  connection cannot be had before the call. Tests: `TestAWithdrawnCallIsNotAnAttempt`, `TestWithdrawnCallsInARowStopTheRunWithoutFailingTheBucket`,
  and on a real cluster `TestRebalanceCallWaitsForTheClusterLockBeyondTheGuardHold` (a session holds `kingres.map_version` in exclusive mode for
  40 s while the call waits).
- **RBR-02, the system database away while a move is in flight.** Every write of the loop goes through `retryRows` (four tries with a growing
  wait, as the checks). While a move is in flight the polls are single tries that are skipped when they fail, and the loop does not return
  before the call has: what became of the move is recorded when it is over (with the retries), so the move is never orphaned and never reported
  as failed for the blip. When the database stays away the run is paused (a safety stop: outcome `stopped` when that can be recorded, else only
  the task is paused) and a start again goes on. Tests: `TestABlipOfTheSystemDatabaseMidMoveDoesNotFailTheMove`,
  `TestASystemDatabaseThatStaysAwayPausesTheRun`.
- **RBR-03, the totals count a bucket once.** A failed, skipped or cancelled row of a bucket that has a later row (a round that a person started
  again repaired it) is history: `rebalance.Current` and the same predicate in `summarySQL` leave it out of progress, summary and verdict. Test:
  `TestTotalsDoNotCountTheRowsOfAnEarlierRoundThatWereRepaired`.
- **RBR-04, what the guard blocks.** An advisory lock replaces the lock on the row (see above): `TestGuardBlocksOnlyTheClaim`. A guard whose session
  ended for another reason than a lost lease (a restart of the system database, the idle timeout) is a void proof, not a lost lease: `RunMove`
  proves the lease again, and only a lease that is gone is `ErrLeaseLost` (`TestVoidGuardOfAValidLeaseIsTakenAgain`,
  `TestVoidGuardOfALostLeaseMakesNoCall`).
- **RBR-05, a move is found by an identity.** Before the call `Backend.Mark` reads the highest move id of the extension; the record of the call is
  the one of the bucket with a higher id. No clock of this process is compared with the extension's. A row left by an earlier holder that never saw
  its record is found by the mark that is kept with the row (migration 0012, `rebalance_move.ext_mark`, written before the call), so that a holder that
  takes over has it and no clock is compared (review ST-05); since P4D-03 the mark is taken before the row and written in the statement that makes it. Only a row made before the mark was kept is found by the time it was started, with a
  tolerance of 10 s for the difference of the two clocks. A recovery of a move that is not settled waits as long as its session is alive (the
  extension's `internal_move_alive`) and for at most `RecoverWait` otherwise (ST-07).
- **RBR-06, rebalance and failover.** The design of the failover (`kingres/docs/design/ha-phase5-part2.md`, section 4.5) stops the rebalance first,
  because a pending or running rebalance excludes a `server.failover` task (migration 0009) and a rebalance with a stuck move (a dead target) would
  hold an automatic failover back. The rebalance offers what that needs: `Service.StopForFailover(ctx, serverID)` sets the flag of a stop with
  `stoppedBy` `safetyRule` and says whether a rebalance was running (a task that is still `pending` has no run yet: ask again), and
  `Service.WaitStopped(ctx, bound)` waits until the task is paused or has ended and says whether it did within the bound. **The stop works while a move
  is stuck**: the flag is read at every poll of a move in flight, while the loop waits for a move that an earlier run left (`Recover`, up to 10
  minutes) and while a call waits for a lock, and each of those waits ends at the flag; the row of the stuck move stays `running` for the recovery of
  the extension and the next start. A move that is switching is not interrupted (the extension ends it within its own bounds). The failover that
  calls this goes on asking, raises its alert after its own limit and never gives up; nothing resumes the rebalance afterwards, a person starts it
  again. Tests: `TestAStopEndsTheWaitForAStuckMove`, `TestStopForFailoverSetsTheFlagAndWaitStoppedWaitsForThePause`.
- **RBR-07, a fence that never clears.** See "The fencing rule of the task engine": a stand-down is logged with its reason, and after
  `task.MaxFencedRuns` runs in a row the task fails with the code `fenced` and the rebalance raises its alert.
- **RB-14 of the first review.** (a) A connection per call: not changed. A poll opens at most three connections on the local socket every 500 ms
  while a move runs; a connection that the loop keeps would have to survive a restart of the instance and a change of the leader, for a gain of a few
  milliseconds a poll. (b) The full plan before every move: not changed, because it cannot be made cheaper from here. `rebalance_plan(max_moves)` limits
  the rows it returns, but the sizes of the buckets are read for every source group before the limit is applied, so a prefix of the plan costs the same
  as the plan; a cheaper plan needs a change of the extension (sizes only for the groups of the first moves). (c) `cleanup_moved_buckets(true)` in the
  removal of a node: not narrowed, because the function has no argument for a group (the request to the extension is `cleanup_moved_buckets(force,
  group)`); it is used only when the removal is refused for a group that takes part in a move, and what it did is now in the log and in the audit log
  (`cleanupMovedBucketsForced`, with the number of moves finished). (d) was fixed in the first round.

## Failover groundwork (increments 0 to 2 of ha-phase5-part2.md)

The failover itself (`failoverServer`, `restorePreferredPrimaries`, the map group bootstrap, automatic failover) is not built yet and
still answers 422 `unsupported_operation`. These increments lay what it stands on; the design is
`kingres/docs/design/ha-phase5-part2.md`, section 10.

### Increment 0: the standbys are in the sharding catalog

What exists now:

- **`standby.PGRegistrar`** (`internal/standby/catalog.go`) implements `CatalogRegistrar` over `kingres.add_node(host, port, group, 'standby')`
  and `kingres.remove_node(node)` of kingres_shard 0.5, on the leader's connection to the system database (it runs the primary of the map
  group). `Register` is idempotent (a standby that is in the catalog answers its node id; the answer `42710` "already registered" is read as
  that) and checks `standby_status()` afterwards (a problem is logged, not fatal). Refusals carry their SQLSTATE (`CatalogError`): `KS007`, `KS008`,
  `KS009` and `55P03` are marked retryable (the next check repeats; a node that did not answer is never asked again inside one call), `KS010`
  is not; kingresd never reads the text. Every call has a deadline of 5 minutes (the extension's bound is `2 x node_timeout + 2 s` per node
  that does not answer): its expiry is "did not finish", repeated by the next check (sequence 9). `SkipCatalog` is gone.
- **The step `standby.register_in_catalog` is `done`, never `skipped`.** It registers every ready standby that has no `catalog_node_id`, not only
  the ones its own task made, stores the node id in `kingres_cp.standby.catalog_node_id` and pushes the list again. The leader's check
  (`work.Register`) also starts a task for ready standbys that are not in the catalog: this is how a cluster made before the registration
  existed converges by itself (test `TestStandbysMadeBeforeTheRegistrationAreRegisteredByThemselves`).
- **The name goes into the list only after the catalog knows the standby.** `Entry.CatalogNode` travels in the plan; the primary's agent
  (`Agent.mayName`) names a standby in `synchronous_standby_names` only when the standby is registered, streams, its name is safe to write
  (`SafeName`: lower case letters, digits and `_`; the extension's check is case-insensitive and treats `*` as every standby) and
  `kingres.standby_name_matches(name)` answers true on the primary. A name that is in the list already stays until the plan drops the standby
  (it was checked when it went in), so an upgraded cluster never flaps. Test `TestMayNameNeedsRegistrationStreamingAndTheExtension`.
- **Removal in the order of the extension's sequence 8.** The plan without the standby is pushed (the primary's agent applies it at once), then
  the standby's server deletes the instance, then `remove_node`; a refusal `KS009` ("cannot commit": the list still names it) is repeated for up
  to a minute (`Leader.UnregisterWait`), and the record of the standby is deleted only after the catalog row is gone (a record that is gone cannot
  be found again to finish the removal). A rebuild that changes the standby's port removes the old row and registers a new one (a node is its
  host and port, sequence 6). `PurgeServer` (a server that is removed or leaves) does the same for every standby it takes along, before the
  membership task unregisters the server's own nodes.
- **The replication view reads the standbys from the catalog.** `pgstore.ManagedStandbyInstanceBase` and `addManagedStandbys` are removed:
  `buildGroup` lists the catalog's standby nodes, finds each one's walsender through the application name of its record (`kingres_standby_<id>`,
  found through `catalog_node_id`) and the instance id of a standby is its real node id. A standby that is created and not yet registered is not
  in the view for the few seconds until it is.
- `membership` ignores standby rows where a primary is meant (`catalogFacts`: a standby of the map group does not make its server the leader).

Still refused: `failoverServer`, `restorePreferredPrimaries` (422 `unsupported_operation`).

### Increment 1: the instance mapping (no behaviour of its own)

Before this increment the code assumed that the primary of a server's group is instance 1 of the server and that the directories from
100 up are standbys. After a failover that is false: a server can run two primaries (its own instance 1 and the promoted standby in
directory `100 + group`), or none (instance 1 became a standby). The mapping below replaces the assumption everywhere; on a cluster that
never had a failover every answer is what it was (instance 1), which is what the existing suites show.

- **Role files** (`internal/instance/role.go`). An instance directory keeps its id for life; `kingres-role.json` next to the marker says
  what it serves: `{group, role: primary|standby, origin: own|copy, term}`. `origin: own` is data this server initialised (its service
  role has this server's password); `copy` is a physical copy of another server's instance, reached by peer authentication on the
  private socket. The file is written with fsync and an atomic replace at creation (`Init`: primary, own, group unknown; `InitStandby`:
  standby, copy, group = id - 100). A directory without a file is what the older layout made of its id (`DefaultRole`); a file that
  cannot be read is an error and is never replaced by the default (`MustRole` falls back to the id's default, which for a directory from
  100 up is a standby and never writable). `cluster.Service.SyncRoles` runs with every lease round (the topology read): it creates the
  files an older layout lacks and names the group of an instance from the catalog row of its port; it never changes the role or the
  origin of a file that exists (a promotion writes those, with the instance it concerns).
- **`cluster.Service`** (`internal/cluster/roles.go`): `LocalInstances`, `WritableInstances`/`WritableIDs`, `PrimaryOf(group)`,
  `InstanceConn`, `PrimaryConn(group, db)`, `SystemConn`, `EachWritable`, `ConnectID`, `AccessSourceID`, `SystemInstance`. `ServiceConn`
  stays "the home instance" except that a server that runs the primary of the **map group** in another directory is reached there (the
  orchestration of the cluster, the catalog and the system database always ask the map primary). Peer or password authentication follows
  `origin`, not the directory id (`IsStandbyDirID` is gone from every decision).
- **Leases per group.** `lease.Config.InstanceFor`, `WatchdogFileFor`, `EnsureWatchdogFor`; the markers are `fenced.<group>` and
  `waitlease.<group>` (`lease.FencedPath`, `MarkerPath`); `lease.Gate(dir, primaryGroup)` holds back only a primary whose own group is
  fenced or waiting; the start of a primary after its lease was won is per group; there is one watchdog per primary instance, each with
  its own lease file (`instances/<id>/lease.json`), lock (`watchdog.<id>.lock`) and stand-down file (`standdown.<id>`, also the old name for
  instance 1 for a watchdog that was started by the earlier version). A marker without the suffix, left by the earlier version, holds
  for every group of the server until the first lease round has read the catalog and renamed it to the groups of the server
  (`migrateLegacyMarkers`); an upgrade never loses a fence and never keeps one for ever.
- **The standby agent per group.** `maintainPrimaries` serves every primary the server runs (the settings, the managed file, the reload and
  the slots of that instance; one `SyncState` per group); `Status.Primaries` carries all of them (`Primary` is the first, for a leader of
  the earlier version); the leader reads any of them (`Status.primaryOf`). `Entry.InstanceID` names the standby's directory; the standbys
  of a server are the directories whose role file says standby.
- **The client entry point** chooses among every primary the server runs (`Env.LocalPorts`): the lowest group whose lease is held, on that
  instance's own port; a catalog primary on this host whose port is not one of the server's primaries is not local.
- **Fan-outs and server-level operations.** Roles, databases and the access rules (`ApplyRoles`, `DropRole`, `ApplyDatabases`,
  `ApplyTrust`, `ApplyRules`, `ApplyStoredRules`) are applied to every writable instance of the server, standbys get them by replication and
  by the copy of the rules (`SyncAccess` from `AccessSourceID`); reads (`CollectRoles`, `CollectGlobals`, the sign-in verifier, the
  proxy's role connections) go to `ConnectID` (the map primary if local, else the first primary). The start, stop and restart operations
  of "this server's instance" act on every primary the server runs (standbys stay the agent's business, as before); the orderly stop
  of the service stops every standby first, then every primary, and the watchdog of each is told to stand down.
- Decisions: the managed settings of an instance other than the home one keep their own port and recovery settings (only the
  certificate paths are rewritten); `cluster.NodeID` stays the name of the home instance; a server whose home instance is a standby and that
  runs no primary uses the home instance for what it must read.

Tests: unit (`internal/instance/role_test.go`, `internal/lease/marker_test.go`, `TestLocalPortsChooseTheHeldPrimaryOfTheLowestGroup`) and cluster
(`TestRoleFilesFollowTheCatalog`, `TestAnOldLayoutConvergesByItself`, and T18 `TestInstanceMappingAfterAHandFailover`: group 1 failed over by
hand with `pg_promote`, the extension's `promote_node` and the role file, after which the server's lease, role files, the replication view and
the fan-out of a role are checked for both primaries; the restart of a server with two primaries is not tested on a cluster, because with the old
primary's server dead a restart of one of the two left costs the majority and the leader rightly fences itself: the markers and `PrepareStart`
per group are covered by unit tests).

Still refused: `failoverServer`, `restorePreferredPrimaries` (422 `unsupported_operation`).

### Increment 2: the lease additions, the read calls, the freeze, the engine gate and migration 0011

Nothing of this starts a failover; it is what the failover task of the next increment calls and what keeps it safe.

- **Grantor** (`internal/lease/transfer.go`): `Grantor.Transfer(group, caller, to, duration)` makes `to` the holder for a fresh duration, only
  when the caller is the holder of record (even before its lease ran out), atomically (there is no instant without a holder, so the
  old holder cannot be granted the group again) and on disk before the answer; `Grantor.Release` deletes the holder's grant. They are
  reached through `POST /lease/grant` with `transferTo` or `release` (`Request`), never for another caller. `GET /lease/state` also
  carries `durationMs` and `marginMs` of the answering server, so the failover can refuse when two servers differ (the argument of
  the README, "Why this is safe", needs them equal).
- **Holder side** (`internal/lease/adopt.go`): `Adopt(group, kind)` renews the lease of a group the catalog does not name this server as
  the primary of (kind `fence`: no instance, nothing is fenced when it is lost; kind `holder`: the server that is about to run the
  primary), `AdoptionStatus(group)` says from the last round whether it is held, how long until enough grantors would grant and who
  refused (a step waits on it; no second protocol), `Attach(group)` makes the lease guard the instance from then on (a loss fences it, the
  watchdog covers it), `Transfer(ctx, group, to)` hands the lease over at a majority and stops renewing (marker `relinquished.<g>`, kept
  across a restart), `ReleaseGroup` is the same with the grants deleted, `Relinquish` stops renewing **but keeps judging** (`Check`
  still fences when the lease that was held ends, minus the margin) and keeps the primary from starting (`lease.Gate`),
  `Unrelinquish` takes it back. A group the catalog names this server as the primary of ends its adoption.
- **The read calls** (`cmd/kingresd/ha_failover.go`, `internal-api.md` "The read calls of the failover"): `GET /failover/position`,
  `GET /liveness`, `GET /failover/state`; read-only, none of them changes an instance. A `GET` carries its parameters as the query string,
  which the internal API now hands the handler as the body of the call.
- **The freeze and `standbys-gone` on the agent** (`internal/standby/freeze.go`): `Agent.Freeze/Unfreeze` (a freeze lapses after its
  ttl, at most 10 minutes, on the monotonic clock) make `SyncState.Decide` leave the list of the group alone; `StandbysGone` takes the standbys
  of a failed server out of the lists of the other groups at once (not of a frozen group). Calls: `POST /failover/freeze`, `/unfreeze`,
  `/failover/standbys-gone` (leader only; in memory only, nothing is written to an instance).
- **The engine gate and the cancel guard** (`internal/task`): `Options.Gate` (while it answers false the engine claims nothing, `RunNow`
  leaves the task pending and a run in progress stands down at the next step boundary, recording nothing and running no hook), and
  `Definition.Cancellable` (the guard of a kind; `Engine.Cancel` and `task.Cancellable`, which the REST layer reports as
  `Task.cancellable`, give the same answer). Nothing in the product sets the gate yet (the bootstrap of the map group does, increment 5).
- **Migration 0011**: `failover_run`, `failover_group`, `failover_event`, `group_replication_state`, `standby.diverged`, and the exclusion
  function (the trigger is the one of migration 0009): `replication.restorePreferredPrimaries` excludes a rebalance and is excluded by it;
  at most one task of the failover class is unfinished (`KX003`); the record of an `instance.stop` or `instance.restart` of an instance
  that is the old or new node of a group of an unfinished failover run is refused (`KX004`, 409 `failover_in_progress`; the instance
  operation then does not run and is not kept in memory). `config.restart` is in the trigger's list but its task has no instance subject,
  so the check never applies to it (it is a task of the engine and a failover pauses nothing it needs).

Tests: T16 (`internal/task/gate_test.go`, `internal/rebalance/failover_exclusion_pg_test.go`), T17 (`internal/lease/failover_test.go`), T19
(`internal/standby/freeze_test.go`), and the read calls on a cluster (`TestFailoverReadCalls`).

Still refused: `failoverServer`, `restorePreferredPrimaries` (422 `unsupported_operation`).

### Review ST-01 to ST-13 of 2026-11-17: what was fixed in this step

Separate from the increments above (own files and tests; see the report of the work).

- **ST-01, the watchdog fenced a healthy primary when its group stopped being enforced.** The invariant now: *a watchdog never fences on a
  lease file that kingresd stopped writing on purpose.* The lease file has a field `off` (`lease.WatchdogFile.Off`, read by
  `cmd/kingres-watchdog`) now means only "this instance is not a primary" (a demotion, `Manager.DisarmWatchdog(group)`): the watchdog never
  fences on it, however old. A group that merely has no standby is NOT off (review FO-01): kingresd writes its file again at every renewal pass,
  whether or not the lease was renewed, with `validMs` of duration minus margin, so the watchdog counts it down like a lease and a dead or
  hung kingresd leaves a file that runs out (a primary that was told "nothing to protect" for ever would run beside a standby that is added and
  promoted later). Every file has `validMs` above zero, also `off` (24 hours), so that a watchdog of the version before `off` (FO-05), which
  ignores the field, never reads zero and fences at once. Tests: `TestTheWatchdogFileFollowsEnforcementBothWays`
  (unit, both directions and the restart), `TestWatchdogDoesNotFenceAfterTheLeaseWasSwitchedOff` (the watchdog itself),
  `TestSwitchingEnforcementOnAndOffNeverFencesAHealthyPrimary` (a cluster: the replica count to 0 and back three times, every instance sampled
  every 100 ms, no fault hook), and `TestRemovingAServerCostsNoOtherGroupItsLease` now runs with real standbys and without `ForceEnforce`.
- **ST-02, an observation that failed was read as "not running".** In `RunMove` an error of `findMove` or of `callRunning` is "unknown": the
  observation session is made again and the loop goes on; a call whose state is unknown is never withdrawn (the bound `maxCallWait` ends the
  wait of a call that was looked at, not of one whose last look failed: P4D-02). Test: `TestABrokenObservationSessionDoesNotCancelARecordedMove` (a real PostgreSQL, the observation session terminated with
  `pg_terminate_backend`, the extension's record inserted afterwards).
- **ST-03, a repeated call repeated its work.** `/standby/rebuild` is a no-op for a standby that is of the generation asked for (the generation
  is in the standby's meta file; the rewind path updates it); `/config/restart` from a task step carries `ifPending` and is a no-op for an
  instance that runs with nothing pending a restart; a request that timed out on the client's side after the connection was made is not repeated
  (`peerclient.clientTimedOut`; a failed dial is). The list of every internal POST handler and what a second execution does is in
  `internal-api.md`, "Repeating a call". Not done: an idempotency key per step run and attempt remembered by the callee.
- **ST-04, the fence waited behind a start.** The lifecycle lock of an instance is a channel that a waiter leaves when its context ends;
  `Manager.Fence` does not take `m.op` (a start holds it); a start that was under way when a fence arrived is stopped again when it returns
  (the marker the fence wrote is there). Tests: `TestTheLifecycleLockWaitEndsWithTheContext`, `TestAFenceDoesNotWaitForAStartAndTheStartDoesNotSurviveIt`.
- **ST-12, statements that were not true:** the text of RBR-05 above is corrected.
- **Not done in this step:** ST-11, ST-13. ST-05 to ST-10 were fixed by the next round (see "Rebalancing after the second review" and the review of 2026-11-18).

### Closing review of phase 4 (P4D-01 to P4D-08 of 2026-11-20): what was done

Review: `kingres/docs/reviews/2026-11-20-kingresd-rebalance-closing.md`.

- **P4D-01, a failover with a move in flight, end to end.** `internal/clustertest/rebalance_failover_test.go`:
  `TestFailoverDuringBucketMove` (the automatic failover with the source of the move lost while the move is held in `copying` and in `switching`),
  `TestSwitchoverDuringBucketMove` (a switchover is refused with `rebalance_in_progress` while a move runs; the stop cancels the move; the switchover then
  runs; the rebalance started again finishes) and `TestMapGroupFailoverDuringBucketMove` (the leader's server, which runs the rebalance and the extension
  session of the move, is lost with a move held in its copy). Every case ends with the same checks: no acknowledged row is lost and no id is there twice,
  every bucket has one owner and the same map is read on every server that is up, no move of the extension is unfinished, `check_cluster()` has nothing that
  forbids a move, and every move of the run's report has an outcome. What the tests do not do: hold the other phases (`preparing`, `catching_up`,
  `cleaning`), and make the standby of the map group replay less than the move's record (it is synchronous); the extension's own TAP test `054` covers the
  failover in the middle of a move for the extension alone.
- **P4D-02, both arms of the ST-02 fix are pinned, and the bound no longer cancels a recorded move.** Tests in `internal/rebalance/p4d_pg_test.go`:
  `TestADeadObservationSessionIsReopenedAndTheRecordIsSeen` (the guard is let go while the call runs, which needs the new session),
  `TestACallThatCannotBeObservedIsNotCancelled` (an error of the running-check is not "not running", and `maxCallWait` does not withdraw a call whose last
  look failed). The decision: the 5 minute bound applies to a call that was looked at. When the last look at the record failed, the record may exist (even in
  `switching`), and cancelling then is the cancel that ST-02 removed; the wait then ends with the call, with the lease (the guard) or with the run. A call that
  is observed and still not recorded after `maxCallWait` is withdrawn as before.
- **P4D-03, the mark is written with the row.** `Backend.Mark` is read before the row is made and `Rows.StartMove` writes it in the same statement
  (`SetMark` is gone), so a row never exists without a mark; the match by time stays for the rows made before migration 0012 only. A mark that cannot be
  had starts nothing and leaves no row. Tests: `TestTheMarkOfAMoveIsWrittenWithItsRow`, `TestAMoveWhoseMarkCannotBeHadLeavesNoRow`.
- **P4D-04, finished tasks are not pruned: a decision, not a fix.** A prune is not a small change: `rebalance_run` and `failover_run` (and their moves, events and
  reports) are keyed by the task id, the audit log and the panel name task ids, and a failover task is the record that a promotion was made. Deleting rows of
  `kingres_cp.task` needs a retention rule that no document has yet (which kinds, how old, whether the report outlives the task row, what the panel shows for a
  report whose task is gone). Until then the growth is bounded at the source: the retry wait of the configuration loop (1, 2, 4, 8, then 10 minutes) makes a server
  that never applies a configuration cost about 144 tasks a day, and other kinds are started by people or rarely. It stays open as a known limit of the task engine.
- **P4D-05, `ifPending` is pinned by a unit test.** `TestARestartCallWithIfPendingRestartsOnlyWhatIsPending` (`internal/pgconfig`, Linux, an agent over a fake `pg_ctl`
  and a stand-in for the question "does this instance have to be restarted now"): nothing pending means nothing is restarted, without `ifPending` it is, with a pending
  setting it is. The cluster test `TestARepeatedRestartCallRestartsOnlyWhatIsPending` stays.
- **P4D-06**, the README row of phase 4, is the owner's to change.
- **P4D-07, a move started by hand and the preflight of a failover: documented, not changed.** `RebalanceRunning` looks at tasks of kind `rebalance` only, so a live move that
  a person started with `kingres.move_bucket` does not make a manual failover or a switchover refuse. The data is protected by the extension (its own recovery settles a move
  whose source or target is failed over, TAP test `054`, and the move worker); what is lost is the operator's early warning. A check that asked the extension for a live move on
  every map group primary would add a call to the preflight of every failover for a case that is rare and safe; it is left for the day a move is started from the panel.
- **P4D-08, one last look before the cancel.** When the run ends before the loop saw a record, `RunMove` reads the record once more (own context, two seconds) and, if the
  extension recorded the move in the last poll, does not cancel the call. Test: `TestARecordWrittenAsTheRunEndsIsNotCancelled`.

### Review FO-01 to FO-16 of 2026-11-17 (the failover increments 0 to 2): what was fixed in this step

- **FO-04 and FO-09, nothing deletes what is not proved to be a standby; a broken role file is not the default of its id.** `MustRole` answers
  `RoleUnknown` for a role file that cannot be read (not writable, not a standby, not a copy; the start gate holds it like a primary). The
  standby agent deletes, rebuilds or rewinds a directory only when `provedStandby` says so: a valid role file that says standby, and
  `pg_is_in_recovery` when it runs or `standby.signal` when it does not (an unfinished copy without a meta file is thrown away unless its role file says
  something else or it runs outside recovery). The refusal is `ErrNotProvedStandby` (409 `not_a_standby`). `Service.PrimaryOf` answers nothing (that is not a protection of either directory; F2-06 below makes the lease fail closed for them)
  when two directories claim one group. Removing the instances of a server that is removed or reset (`RemoveInstance`) is the server's own removal
  and is not guarded. Increment 3 writes the role file before `pg_promote`.
- **FO-02.** `SyncState.Decide` never changes the list of a frozen group, also when no name is planned.
- **FO-01 and FO-05.** See the paragraph of ST-01 above.
- **FO-03.** The start gate of an instance whose group is not known (`instance.GroupUnknown`) is held back by the marker of any group.
- **FO-06, FO-07, FO-10, FO-13.** A transfer is done at a grantor when the holder it names is the target (a repeat after a lost answer is done);
  a release is done when it names no holder; a grantor of the version before them names the caller and counts for nothing, so a hand-over to
  old grantors is refused. `Transfer` and `ReleaseGroup` refuse while an instance of the group runs on the server. `Adopt` clears the marker of an earlier
  hand-over (the failover that adopts decides). `ReleaseGroup` marks the group, waits for the round in flight (`rmu`) and only then asks the grantors.
- **FO-08.** The read-back of `add_node` and `remove_node` reads the row on every primary of the catalog (`PGRegistrar.Remote`); a primary that cannot
  be read is a refusal that the next check repeats. `Unregister` compares the port as well as the host.
- **FO-11.** `Engine.Cancel` is refused when the task cannot be read; a run asks the guard of the kind again at the boundary and ignores a cancel mark
  past the point of no return. Migration 0013: an instance stop or restart is refused (KX004) while its server is the old or the new server of an
  unfinished failover group, and a `config.restart` while any failover group is unfinished.
- **FO-12.** Freezes and "gone" servers are kept in `failover-freeze.json` in the data directory and read again at the start.
- **FO-14.** `Manager.start` asks the start gate again when pg_ctl is done and stops an instance that came up against it.
- **FO-16.** `StartInstance`, stop and restart try every primary of the server; a watchdog started for another group than the instance serves now is ended
  before the right one is started.
- **Not changed.** FO-15 (a name in the list of a standby that is never registered): increment 3 refuses a failover while a named standby is not in the
  catalog. FO-16, the removal that deletes the instance before the primary applied the list without it, stays bounded by the grace period.
- **Fencing under a stall** (`internal/clustertest/fencing_stall_test.go`): two grantors stalled with SIGSTOP for longer than the margin fence the third
  server's primary within the bound, nothing becomes a primary, and it comes back by itself; a stalled leader fences only its own primary.

## Failover of an ordinary group (increment 3 of ha-phase5-part2.md, contract 0.10.0)

`POST /servers/{serverId}/failover` with `mode: hard` fails over every shard group whose primary runs on the server (or the groups of
`shardGroupIds`), one at a time, as the task `server.failover`. Refused with `422 unsupported_operation` in this increment: `mode: graceful`, the map
group (group 0), `restorePreferredPrimaries`, `resetAutoFailoverCounter`, automatic failover (the setting is accepted and does nothing, as before).
Code: `internal/failover` (the task, the choice, the repeat loop of `promote_node`, the report), `cmd/kingresd/failover.go` (the ports over the peer client,
the extension and the records, and the callee side of the calls), `internal/standby/failover.go` (`Repoint`, `EmptySyncNames`), `internal/httpapi/handlers_failover.go`.

### The task, step by step

The steps of a group are named `Group <n>: <step>`; each is a no-op on its second execution (it reads what exists first) and a time-out of a call is
read as "the call did not finish": the step asks the callee for its state and then decides.

| Step | What it does, and what it does on a repeat |
|---|---|
| `Check that the failover can start` | The refusals of the request again (`Preflight`), then the run's records and, for a dead server, `standbys-gone` to the other primaries. A repeat finds the record of the run and does nothing. |
| `Freeze the group` | Sends the freeze to the old primary's server and reads it back with `GET /failover/state` (it lapses, and a wall clock set forward can end it early); every later step that relies on it sends it again. An unreachable server needs none. |
| `Stop the old primary` | `POST /failover/demote`: relinquish, immediate stop, confirmed, and only then the hand-over of the lease to the leader. Skipped when the server does not answer (the lease fences it). **Irreversible: the task is not cancellable from here (`Task.cancellable` false); this is earlier than the design's promotion, because from the stop on the group has no primary.** |
| `Wait for the old lease` | The leader adopts the lease as the fence holder and waits (25 s) for a majority of grantors; a lease call that keeps failing with an error of its own for 25 s pauses a failover and rolls a switchover back (`lease_unavailable`), and the freeze of the group is renewed every 30 s while the step waits; `lease_still_held` when the grantors still renew the old holder, `lease_held_elsewhere` otherwise; without a majority it waits a minute and pauses. "Fenced" is the lease, never a timeout of the step. |
| `Choose the standby` | Reads the positions of every ready standby twice, a second apart, until they stop moving; a standby out of recovery is a promotion already made (pinned; two of them stop the task with `split_promotion`); `Choose` orders by the larger of the received and the replayed position (the received one is empty when no walreceiver has run), then synchronous state, preferred server, ring, id. The verdict (`none`, `asyncOnly`, `possible`, `unknown`) must be accepted by `allowDataLoss`, or the task pauses with the alert `failoverNeedsDecision`. The choice is pinned once in `failover_group` and a repeat never chooses again. |
| `Hand the lease to the new server` | The new server holds the lease before it is promoted: a transfer from the fence holder, then `holder` adoption on the new server (3 tries of 15 s). A repeat sees that the new server already holds it. |
| `Promote the standby` | `POST /failover/promote`: empty list, role file (primary) written first, lease checked as the last thing before `pg_promote`, then `Attach` (watchdog). `lease_not_held` goes back to the hand-over once; `not_standby` withdraws the pin. **Irreversible, not `NonIdempotent`.** |
| `Make the catalog follow` | `kingres.promote_node` repeated by the README rules of the extension (`promote.go`: 10 calls, 15 minutes, the answers by SQLSTATE; `KS007` only after a probe, `KS014` and `KS012` ask the operator, `KS009` for the new primary re-applies the empty list). |
| `Check the catalog` | Reads the result back (a success of `promote_node` is not proof for a node that went silent at the end): the new node is the primary, the old one is not, the term rose; repeats the call once, then pauses. Stops an old primary that is still running (`oldPrimaryRunning`). |
| `Point the other standbys at the new primary` | `POST /standby/repoint` per standby (slot on the new primary, `primary_conninfo`, wait for the walreceiver); a standby that does not stream is flagged for a rebuild. |
| `Update the standby records` | One transaction that proves the lease of the run: the record of the promoted standby is deleted, the others follow the new primary, the old primary is inserted as a standby that waits for its rebuild (increment 4, below); the plan is sent. |
| `Check synchronous_standby_names`, `Resolve prepared transactions`, `Check that clients arrive` | Waits (bounded) that never fail the task; `recover_prepared_transactions()` and `check_cluster()` on the map group primary, incidents into the report, the alert `failoverDataLoss`. |
| `Lift the freeze`, `Write the report` | Unfreeze, group `done`, the run's outcome. |

The records are `failover_run`, `failover_group` and `failover_event` (migration 0011); every write proves holder, epoch and lease of the task run
(`PGRows`), so a superseded run records nothing. The standby leader leaves the groups of an unfinished run alone (`Leader.Busy`), and records every 10
seconds what it saw of each group's primary (`group_replication_state`: flush position, whether a synchronous standby streamed), which is the "last
known position of the old primary" of the report and the base of the verdict.

### Decisions of this increment

- The control calls (`promote_node`, `recover_prepared_transactions`, `check_cluster`, the reads) each open a session of their own at READ COMMITTED with
  one statement and a named deadline (`promoteNodeDeadline`, `shortControlDeadline`) derived from `kingres_shard.node_timeout`.
- The internal API is not versioned for the new calls: an older server answers `404 not_found`, which is "older version" (the request is refused with
  `version_mismatch` when a member does not report its lease settings), and `no_instance` stays a real answer.
- The verdict reads the durability of every user database for every group (a sharded database has buckets in all groups; a database that is not
  sharded lives in the map group).
- A failover is refused while a rebalance is pending or running (`rebalance_in_progress`): this increment does not stop it for the operator.
- `recover_prepared_transactions()` runs on the map group primary and on the promoted instance (`POST /failover/prepared`).
- Not done in this increment: a rollback of a failover that was cancelled after the old primary was stopped (not offered), the
  rebuild of the old primary (done in increment 4, below).

## The old primary after a failover (increment 4 of ha-phase5-part2.md)

After a failover the old primary becomes a standby of the new primary, by itself when its server returns, and a replacement is made when it does
not. Still refused with `422 unsupported_operation`: `mode: graceful`, the map group, `restorePreferredPrimaries`, automatic failover.
Code: `internal/standby/oldprimary.go` (the proof, the order of the rebuild, the divergence), `internal/standby/leader.go` (`watchOldPrimaries`, the
routing of a `diverged` record to a rebuild, the ring), `internal/instance` (`Rewind`, `RewindDryRun`, `RebaseStandby`, the mark of an unfinished copy),
`internal/lease` (`HeldBack`, `BecameStandby`), `internal/failover` (the record of the old primary in `records`, the live state in the report),
migration 0015, `cmd/kingresd/ha_leases.go` (the hooks, the alerts, `fenceStandbyPrimaries`).

**The record.** The step `Update the standby records` also inserts, in the same transaction, the old primary as a standby of the new one: state
`rebuilding`, `diverged`, generation 1, the directory of its instance when its server answered (else 0: the server finds the directory by its role
file and the answer of the rebuild says which), the port of its catalog node. The catalog row is the one the extension kept (role `standby`
after `promote_node`); the registration step reads it back when the standby is ready. The report's `oldPrimary.state` is read live: `rebuildScheduled`
(before the first try), `rebuilding` (the standby task runs), `waitingForServer` (its server does not answer the leader), `standby` (rebuilt),
`removed` (the record is gone: the replace-after rule or the removal of the server). `method`, `divergenceBytes`, `rewindBytes` and `message` come
from the answer of the rebuild (`failover_group.old_primary_report`, migration 0015).

**The rebuild** (`POST /standby/rebuild` with `diverged` and the catalog `term`). The directory of the old primary is a primary on disk; the server
touches it only when it proves the following: the role file says primary of exactly this group, the instance is stopped, the lease of the group is not this
server's and is not being adopted, a marker of a hand-over, a fence or a wait is on disk, the plan names another server as the primary of the group, and
the term is newer than the one in the role file. Then, in an order that survives a crash at every point: (1) the mark `kingres-copy-unfinished` (with its
step) and the role file `standby, copy` are written; the slot on the new primary is made; (2) `pg_rewind --dry-run --progress` measures the divergence (where
the histories parted, the bytes it would copy; the shutdown checkpoint that its crash recovery leaves is the end of the old WAL, so
`divergenceBytes` is the end of the old WAL minus the divergence point); (3) `pg_rewind` (it finishes the crash recovery of a primary that was fenced with an
immediate stop); (4) when it fails (the fault flag `failRewind` makes it fail in the tests), the data directory is replaced by a base backup of the new
primary into the same directory (`RebaseStandby`: marker, role file, settings, trust stay; the home instance of a server is such a directory); (5) the
standby's own managed settings are written again (`pg_rewind` copies the new primary's over them: port, socket, certificate paths), the access rules are
copied, the meta file written; (6) `lease.Manager.BecameStandby` removes the markers of the group and the memory of the hand-over and sets the watchdog
file to `off`, then the mark goes, then the instance starts. A rewind that was interrupted (mark step `rewinding`) is repaired by a base backup, one that
finished (`rewound`) is not done again. A standby that is ahead of its new primary (`FlagRebuild`) goes through the same steps with the proof of a
standby. Refusals: `409 not_former_primary`, `409 old_primary_running` (the leader's demote stops it first), `409 not_a_standby`.

**The server that does not return.** The leader notes the first check that finds the server silent (`standby.waiting_since`) and clears it when the
server answers. After `standby-replace-after` (default 30 minutes) the record is marked `removing`: the primary's list stops naming it, the instance
is removed when the server returns (its supervision removes standbys that are not in the plan; the old primary's directory is not a standby and stays,
stopped and held back), the catalog row goes by `remove_node` (extension sequence 8: the list first), and the ordinary reconcile makes a replacement on
a server that is not dead (`Leader.Dead`: a dead server gets no new standby, and a standby that is on it stays). The alert `oldPrimaryNotRebuilt`
(warning) comes after the shorter of ten minutes and half of `standby-replace-after`, counted from the record, whether the server is silent or answers
and the rebuild does not succeed; the rebuild clears it, and `oldPrimaryRunning` and `oldPrimaryNotStopped` of that server and group.

**The ring.** `readTopology` puts every active server in the ring, not only the servers that run a primary: after a failover a server can run only
standbys and is still a place for the standbys of other groups; without this the record of the old primary would have found its server no candidate
and been removed.

**An old primary that runs.** `fenceStandbyPrimaries` (every 2 s) stops an instance whose role file says primary, on a port that the catalog calls a
standby, while this server does not hold or adopt the lease of its group (twice in a row): the old primary of a failover whose server had neither a
working `kingresd` nor a watchdog (test T11c), or a promoted instance whose `kingresd` restarted before the catalog followed. The probe of the
monitor raises `oldPrimaryRunning` for the same reading.

**Configuration.** `standby-replace-after` (flag, `KINGRES_STANDBY_REPLACE_AFTER`, file key; default `30m`) joins the keys of the table above.
Fault hook: `failRewind` (tests only).

**Tests.** Unit (`internal/standby`): `TestAPrimaryThatIsNotProvedToBeTheFormerPrimaryIsNotRebuilt` (seven refusals, nothing touched),
`TestTheFormerPrimaryIsRewoundAndBecomesAStandby`, `TestTheFallbackIsABaseBackup`,
`TestAnInterruptedRewindIsRepairedByABaseBackupAndAFinishedOneIsNotRepeated`, `TestTheDirectoryOfTheFormerPrimaryIsFoundByItsRoleFile`,
`TestDiffRoutesAnOldPrimaryToARebuild`, `TestDiffPlacesNoNewStandbyOnADeadServer`, `TestTheLeaderWatchesTheOldPrimariesThatWait` (PostgreSQL),
`TestTheReportOfTheOldPrimaryIsStoredInTheNewestRunOfTheGroup`. Cluster, in WSL (`internal/clustertest`): T11 `TestOldPrimaryReturns` (pg_rewind, the
rows only the old primary had are gone, markers and mark gone, streaming, the new term, `check_cluster()` empty, the report, then a second failover
back to it) and `TestOldPrimaryReturnsAndIsRebuiltByABaseBackup`; T11b `TestOldPrimaryNeverReturns`; T11c `TestOldPrimaryRunningUnfenced`;
`TestAStandbyThatLagsIsNotChosenAndFollowsTheNewPrimary`; `TestASecondFailoverWhileOneIsPaused`.

## Fixes for the independent review F2-01 to F2-17 (2026-11-18)

Done, each with a test that fails without it: **F2-01** (`SyncState.Decide` returns no change at all for a frozen group; `f2_01_test.go`),
**F2-12** (the freeze carries task and epoch, a lower epoch of the same task is refused with 409 `fenced`, a freeze that cannot be written
is an error to the caller, the owner survives a restart; `f2_12_test.go`), **F2-03** (the release writes its marker before its first
request), **F2-04** (`Adopt` keeps the marker file, so the old primary directory stays held back; `Attach` and `Unrelinquish` remove it),
**F2-11** (`Attach` before the lease is held is an error), **F2-05** (a stop that failed is repeated every `FenceRetry`, `Enforced` is read at
every check, `Config.OnFenceFailed` is called on every failure; the alert itself is not wired), **F2-07** (the watchdog file of a group that is
not enforced is refreshed by a loop of its own, the rule being: a group without a standby is stopped for a lapsed lease by nothing, only for a
kingresd that stopped running), **F2-10** (the read-back also happens in the `42710` branch, and a dead server is skipped instead of blocking),
**F2-13** (`lease.LockHeld`, `GET /failover/state` reports `watchdog`, the failover refuses an old primary that runs without one, no fork
while an old watchdog holds the lock), **F2-17** (the cause is not the clock; see below).

**Closed in the round of increment 4 (2026-11-18).** Each with a test that fails without it:

- **F2-02** (`TestARoundIsNotSentForAGroupReleasedInTheMiddleOfAPass`): two groups on one server, `Renew` has taken the list `[1, 2]`, the release of
  group 2 starts inside the first request of the round of group 1; the pass must not send a renewal for group 2 (the check `mayRenew` under the round
  lock). It fails when the check is removed.
- **F2-06** (fail closed): `cluster.Service.Unattributed(group)` lists the instances that may be the primary of a group that the role files cannot
  attribute to it (an unreadable role file, two directories claiming the group, a role file that names another group than the catalog gives for the port);
  `lease.Config.Unattributed` makes `Fence` stop them with the instance of the group, `refence` count them, and `Transfer` and `ReleaseGroup` refuse while
  one runs; `GateInfo` judges a primary whose role file contradicts the catalog as one of any group, so any marker holds it back. The warning is logged once
  a minute per group (the contract has no alert kind for it). Not covered: a watchdog for such a directory (it has no lease file name).
- **F2-08**: an unfinished copy is the mark `kingres-copy-unfinished` that `InitStandby` writes first and the agent removes last (a meta file that cannot
  be read is no longer the sign of one; the directories of an older version have no mark and are not deleted); `provedStandby` asks the instance through
  `Agent.AskRecovery`, so the two legs "running and not in recovery" and "running and cannot be asked" have tests; the promotion of a standby takes the
  agent's lifecycle lock (`Agent.LockLifecycle`) for its whole run.
- **F2-09, the other direction**: `Ops.EnforcedPrimary` (wired to the lease manager's view: a writable instance whose group has a standby) makes a stop
  or restart that cannot be recorded fail with `503 service_unavailable` instead of running from memory; a start still runs from memory.
- **F2-05, the alert**: `Manager.FenceFailures` and a monitor probe raise `oldPrimaryNotStopped` while a fence cannot stop a primary and clear it when the
  stop succeeds or the lease is won again.
- **F2-14**: the statements of this README about the fence marker, the enforcement rule and `PrimaryOf` are corrected above.
- **F2-16**: `TestAReleaseWaitsForTheRenewalInFlight` waits for the release to have marked the group instead of a fixed time;
  `Recover` of a bucket move logs once when `internal_move_alive` does not answer; `TestTheRealExtensionAnswersTheMoveAliveCallOfTheRecovery` calls the real
  function on a real map primary. Two groups on one server are in the lease tests now (the F2-02 test).

**The 503 of the clock subtest (F2-17).** The cause found in the logs of a failing run on PostgreSQL 19 is not the restart delay the review
read: node 1's kingresd lost the lease of its group and fenced the primary (`pg_ctl` immediate shutdown at 06:16:38.751), and in the same
instant a round won the lease again and started the instance, which was still running, so the start did nothing; the stop then ended it, the
markers the start had removed left nothing to start it again, and the instance stayed down although the lease was held (the log said "the
group lease is held: the primary instance was started" and the following reads were refused with connection refused for thirty seconds). The
clock was not involved. Fix: `Manager.Fence` marks the group as being fenced until its stop has ended and `acquired` does not start the
instance while it is (the marker stays, the next round starts it); `TestARoundThatWinsTheLeaseDuringAFenceStartsTheInstanceAfterTheStop`. The
subtest also waits for the lease to be freshly held before it steps the clock.

## Fixes for the independent review F3-01 to F3-16 (2026-11-18, contract 0.12.0)

The review is `kingres/docs/reviews/2026-11-18-kingresd-failover-increment-3.md`. Done, each with a test that fails without it (the mutants
are listed in the report of the round):

- **F3-01** `lease.Manager.Unadopt` refuses an adoption that is attached (it guards a promoted primary) and returns false; the handler of
  `POST /failover/lease` with role `release` answers `{held: true}` then. A failed task (`OnFail`) can no longer make the lease of a promoted
  primary run out when the leader's own server is the new primary. Cluster test `TestFailoverFailedTaskKeepsTheLeaseOfThePromotedPrimary` (the
  failure is injected after the promotion with the fault `failoverPoint`, mode `fail`); unit test `TestUnadoptRefusesAnAttachedAdoption`.
- **F3-02, F3-12** `GroupFacts.PrimaryPlanned` (migration 0018, `group_replication_state.planned_standbys`, filled by the leader's check from
  `PrimaryStatus.Planned`, which the primary's agent reports from the plan it holds) is what the leader knows of an old server that does not answer.
  `Preflight` refuses (409 `cluster_inconsistent`) a failover of such a server without proof that its plan carried a standby, unless the request
  says `allowDataLoss`; `dryRun` shows it as a blocker. `GET /failover/state` reports `enforced`. The other half of the finding (a lease that
  fences regardless of the plan) is **not** done. Cluster test `TestFailoverRefusedWhenFencingOfTheOldPrimaryIsUnproven`.
- **F3-03** `failover.UnobservedAge`: the age of the last check is lowered only by the silence that a majority of the members share, and only by
  what exceeds the lease and its margin (the leader asks the other members with `GET /liveness`). Unit test `TestUnobservedAgeSubtractsOnlyWhatAMajorityDidNotHear`,
  cluster test `TestFailoverVerdictIsNotNoneWhenOnlyTheLeaderLostTheServer`.
- **F3-04** the request refuses a database whose catalog lacks the old primary (host and port), as it already did for the standbys; migration 0016
  makes a failover and `database.create`, `database.drop` and `table.distribute` exclude each other (KX006 for the failover, KX007 for the database
  operations; `task.excluded`, `MemStore`); a failure of `targets` after the promotion is a pause (`targetsPause`), never a failed task. Tests:
  `TestADatabaseWithoutTheOldPrimaryRefusesTheFailover`, `TestADatabaseThatAppearsAfterThePromotionPausesTheTask`,
  `TestAFailoverAndDatabaseChangesExcludeEachOther` (PostgreSQL), cluster test `TestFailoverExcludesDatabaseCreationAndPausesAfterThePromotion`.
- **F3-05** `promoteAction`: a role file `primary` on an instance still in recovery is a promotion to finish, not `not_standby`.
- **F3-06** `choose` keeps a pin whose standby can be read (the accepted verdict is not recomputed for another one) and withdraws a pin whose
  standby cannot be read while nothing is promoted.
- **F3-07** F2 stops a live old primary only after one ready standby was read in recovery (`someStandbyReadable`).
- **F3-08** `promote` answers 409 `lease_not_held` when `Attach` fails (both paths).
- **F3-09** the client entry point reads the catalog from another server (`GET /catalog/nodes`) when its own cannot be read. Cluster test
  `TestFailoverEntryPointOfTheFailedOverServerForwards`.
- **F3-10** F7 and F11 run `recover_prepared_transactions()` and `check_cluster()` in every database that has a catalog (`Catalog.RecoverPrepared(ctx, db)`).
- **F3-11** a standby found out of recovery that this task did not pin pauses the task (alert `splitPromotion`) and is adopted only with
  `allowDataLoss`, with a recorded verdict `possible`; the summary counts a promoted group without a verdict as a possible loss.
- **F3-13** `group` must be in the body of `lease`, `demote`, `promote` and `prepared`; `demote` and `promote` need the fence header and are
  refused with 503 when the callee cannot read its catalog term. Cluster test `TestFailoverCalleeRefusesUnfencedAndUnnamedCalls`.
- **F3-14** an unfreeze lifts only a freeze of its own task; `OnFail` sends its own task with the highest epoch.
- **F3-15** the outcome `completedWithWarnings` (migration 0017, contract 0.12.0) when a step recorded a skipped standby, a pending list, remaining
  prepared transactions or an incident.
- **F3-16** the test list: the cluster tests above cover a live old primary only through F3-09, the leader's server as the new primary (F3-01), a
  failure after F6 (F3-01, F3-04), a database created during a failover (F3-04). Not covered: a lagging standby, a second failover while one
  is paused, a lost answer of any call, a kill of the instance with the writers still running (the old tests stop the instance cleanly).

Also fixed in this round: **the renewal rounds of the lease manager left requests in flight.** A round ends when a majority answered; the request to
the last grantor was still on its way and could reach the grantor after a release (`ReleaseGroup` waited for the round, not for its stragglers: the
claim of FO-13 was not true) and could write into a test's directory after the test ended. `internal/lease/flights.go` counts the requests of
rounds, releases and transfers per group; `ReleaseGroup` waits for those of its group, and the rig of the tests waits for all before its directories are
removed. This was a defect of the lease code (the intermittent `TestAReleaseWaitsForTheRenewalInFlight`) and of two tests that counted requests
of the round before (`TestARoundIsNotSentForAGroupReleasedInTheMiddleOfAPass`). The `TempDir RemoveAll cleanup: ... not empty` that remains on Windows
when the whole package runs (and never when a test runs alone, 40 of 40) is the outside-process problem of this machine, not a writer of ours.
Two cluster tests read once where the state is in motion and took a 503 as final (`topology` right after the formation, `GET /tasks/{id}` on a
restarted leader that has not found itself again): the 503 with `Retry-After` is the answer of the product (a member that does not know the manager
leader says so, a server whose instance is starting says so), so the helpers wait for 200 within a bound.

Fault injection for the tests (never in a product's configuration): `fault.Spec.FailoverPoint` and `FailoverMode` (`fail` or `pause`) fail or pause the
failover task at `<step name>:before`.

## The map group (increment 5 of ha-phase5-part2.md): what exists, and what does not

Built (contract 0.13.0): `internal/failover/journal` (the local journal), `internal/failover/mapgroup` (the bootstrap B1 to B8 over ports, tested with fakes at every step boundary), `cmd/kingresd/mapfailover.go` (the real ports, the candidate, the receiver, the answers about the bootstrap), `failover.RecordMapGroupRun` (the recorded task), `rebalance.PauseForMapGroupFailover`, the engine gate (`task.Options.Gate` from the journal) and the calls `POST /failover/map/start` and `/continue`. A request for the failover of the server that runs the primary of the map group is answered by any server that does not hear it (`httpapi.MapFailoverer`) with the task with id 0.

**Not built here (built since, see "Escrow of the key of the certificate authority"):** the escrow of the key of the certificate authority (without it an alert, `certificateAuthorityUnavailable`, says what the operator does by hand: restore `pki/ca.key` and `pki/revoked.json` of the old leader from its backup into the pki directory of the new leader and restart kingresd), the second condition of the gate (the instance of group 0 in recovery), `graceful` for the map group. `Preflight` still refuses the map group with 422 `unsupported_operation` when the leader is alive (the request then goes to the leader).

**The map group with a live old leader (a switchover, a restore, a hard failover).** A planned switchover of the map group (`failoverServer` with `mode: graceful` for the leader's server, and a `restorePreferredPrimaries` that has to move group 0) is built: the map group is the last group of the task; its step `Stop the old primary cleanly` first starts the bootstrap of the chosen standby's server (`POST /failover/map/start` with `planned`, accepted from the old server only), then marks, lets the standby replay the mark, stops the old leader with a fast shutdown, and sends the candidate the proof (`POST /failover/map/proof`); the candidate takes the released lease, promotes on the proof after `switchover_verify`, becomes the leader, marks the task's group 0 row promoted with the new term and opens the gate, and the new leader's engine claims the task and runs again the steps that the old leader could not record (the clean stop, the verification, the lease and the hand-over, the promotion skip because the standby is out of recovery). A **hard** failover of a server whose map group primary answers is built too (contract 0.28.0, see the next paragraph); the bootstrap itself still refuses a live old server unless the old leader's own task asks (409 `conflict` for an automatic start, 422 `unsupported_operation` for a manual one), and never asks the old server to demote. When the standby refuses the proof after the stop, the candidate lets go of the lease and tells the old leader, whose task rolls back and whose primary is started again by its lease manager; when the old leader's kingresd ends after the stop, the candidate reads the proof from the old leader's server and promotes on it (`docs/design/ha-phase5-part2.md`, "The two open points of the delta, settled").

**A hard failover of the map group while its server is alive (phase 5 closing, contract 0.28.0).** `failoverServer` with `mode: hard` for the leader's own server (`Input.MapLive`, set by `Resolve` when the request includes group 0) is no longer refused: the map group is the last group, and its step `Stop the old primary` (`internal/failover/maplive.go`, `stopOldMap`) does, in this order, (1) judges the loss with the choice of F4 before anything changes (a verdict that the request did not accept pauses the task with the old leader running; no readable or ready standby fails the step with nothing changed), (2) starts the bootstrap of the chosen standby's server with `POST /failover/map/start` and `planned.hard` (the task id, the standby, the acceptance of the operator and the leader's facts as of the stop: `planned.facts`), (3) fences its own primary with one call to its own server, `POST /failover/demote` with `mode: immediate`, `transferTo` and `candidate` (`fencedStop` in `cmd/kingresd/failover.go`: the candidate is recorded, the lease renewal ends and the marker is written, the instance stops with an immediate shutdown, and only when it is seen stopped is the lease handed to the candidate), and (4) waits, on a context that the engine does not cancel, for a standby of the map group to be out of recovery. The candidate waits at B3 for the lease (4 minutes, the bound of a planned switchover), ranks all the standbys after the stop with the leader's facts, promotes with no proof, and marks the old leader's task with the pin and the verdict (`RecordPlanned`); the new leader's engine skips the stop, the lease, the choice, the hand-over and the promotion and runs the rest. A primary that does not stop keeps a lease nobody renews: the watchdog stops it when it runs out and the candidate takes the lease after that (test fault `demote:nostop`); kingresd of the old leader may end after the fence (`demote:exit`). A loss that shows only after the stop pauses the bootstrap (`failoverNeedsDecision`, the message names the server) and the repeated request goes to the standby's server (`mapFailover.Handles` takes it while a hard bootstrap is in progress there). The fencing argument is in `docs/design/ha-phase5-part2.md`, "A hard failover of a live leader's server". Cluster tests: `TestHardFailoverOfTheMapGroup*` (`internal/clustertest/failover_maphard_test.go`, with a sampler of the map group that fails at two writable instances).

## Escrow of the key of the certificate authority (contract 0.26.0, ha-phase5-part2.md 3.4.1)

Built: `internal/pki/escrow.go`, `internal/membership/escrow.go`, the adoption in `cmd/kingresd/mapfailover.go` (`wireEscrow`, `CheckAuthority`), the operations `getCaEscrow`, `setCaEscrow`
and `rotateCaEscrow` (`internal/httpapi/handlers_members.go`, type `CAEscrow`), the internal calls `POST /pki/escrow/offer`, `store` and `drop` (`LeaderOnly`, see `internal-api.md`).

- **What it is.** The manager leader keeps a sealed copy of `ca.key` and of the revocation list on each server that hosts a ready standby of the map group. When the map group is failed over, the new leader
  opens its own copy at B8, writes `pki/ca.key` and `pki/revoked.json` (the revocation list is the union of the copy and the list the old leader last distributed), and issues, renews and revokes
  certificates at once. No alert is raised and nobody restores a file. A copy is sealed to an escrow key of its server (X25519 in `pki/escrow/recipient-<id>.key`, mode 0600, not the TLS key) with
  HKDF-SHA256 and AES-256-GCM; the additional data bind recipient, key id, CA fingerprint, generation and time.
- **Off until switched on.** `PUT /certificate-authority/escrow` with `{"enabled": true}` (`kingres_admin`). It is an operator decision: every recipient's disk can then hold the CA key. `GET` shows the recipients
  and whether each holds the newest copy (`complete`); `POST /certificate-authority/escrow/rotations` makes every recipient a new escrow key and reseals a new generation. `enabled: false` deletes every copy
  (a server that cannot be reached is asked again every minute).
- **Review fixes (2026-11-23).** Revocation lists never shrink (a holder refuses a list that takes a revocation back, 409 `stale_revocations`; a leader merges the lists of every server before it distributes; the list number is at least the time in seconds); copies are ordered by (epoch, generation, seq) and a leader that takes over takes a higher epoch, so a former leader cannot override or delete; the records of recipients stay as `dropPending` until a drop succeeded; the operator's choice and the holders are mirrored to `kingres_cp.cluster_setting` (`caEscrow`) so a new leader keeps them; `ca.key` is written durably and a truncated one is replaced. See the report in `docs/reviews/2026-11-22-ca-key-escrow.md`.
- **Kept current.** The leader reconciles every minute (`membership.Manager.RunEscrow`): it sends to a recipient whose copy is missing, older than the revocation list, of an older generation or older than 6 hours,
  takes the copy away from a server that is no longer a recipient (removed, revoked, standby gone), and sends again at once after every revocation.
- **What a recipient checks.** It stores a copy only if it opens with its escrow key, holds the key of the `ca.crt` the server already has, and is not older than the copy held; at adoption the same check is made again,
  so a blob somebody made for the server, a damaged one or a replayed old one gives no key (the alert is raised, nothing is written as `ca.key`).
- **Threat model.** A recipient's disk holds the copy and the key that opens it: its compromise is a compromise of the CA, as the leader's disk always was; the escrow widens the set of such disks from one to the
  leader's plus the standbys of the map group. A server removed while it cannot be reached keeps its copy and must be treated as knowing the key; the rotation of the CA itself is not built. The key is never in
  a REST answer, task, audit event, log or table. See the design for the alternatives that were rejected.
- **Not built:** an alert for a recipient that stays out of date (the status shows it); the removal of `ca.key` from an old leader that comes back. The console shows and changes the escrow on the Certificate authority tab of the Security page.
- **Tests:** `internal/pki` (`TestEscrowRoundTripGivesTheNewLeaderTheCA`, `TestHostileBlobs`, `TestAWrongKeyOpensNothing`, `TestABlobForAnotherCAIsNotAdopted`, `TestAnOlderBlobDoesNotReplaceANewerOne`,
  `TestRotationReplacesTheEscrowKeyAndTheOldOneStopsWorking`), `internal/membership` (`escrow_test.go`: the leader's side with the receivers of other servers), `internal/httpapi`
  (`handlers_caescrow_test.go`), and the cluster tests `TestCAEscrowMapGroupFailover` and `TestCAEscrowTamperedCopyIsNotAdopted` (`internal/clustertest/caescrow_test.go`).


## Automatic failover (increment 6 of ha-phase5-part2.md, no change of the contract)

Code: `internal/failover/auto.go` (`Evaluate`, the rule of design 4.2 and 4.3 as a function of a view; `Auto`, the loop every 5 s on the manager leader),
`cmd/kingresd/autofailover.go` (reads the view: the settings from `cluster_setting`, the tracker, the corroboration of the members by `GET /liveness`, the
standbys, the unfinished task, the end of the last failover per group; stops a rebalance before the start) and `cmd/kingresd/mapmonitor.go` (the candidate
monitor of the map group on a server that hosts a ready standby of group 0: rank by standby id, 30 s of waiting per rank, a new look at the same leader and
term, then the bootstrap with the trigger `automatic`, never with `allowDataLoss`).

A server is failed over when its tracker says dead for at least `max(timeoutSeconds, 3 x failure-timeout)`, a strict majority of the members report it
not live, this server hears a majority, no failover task is unfinished, and none of its groups was failed over in the last 5 minutes. A failed start backs off
60 s doubling to 10 minutes. `autoFailover.eventsSinceReset` is incremented once per automatic task in the transaction that records its first promoted
group; at `maxCount` the loop raises `autoFailoverLimitReached` once; `resetAutoFailoverCounter` and a completed rebalance set it to 0. The end of an
automatic run raises `autoFailoverTriggered`; the start writes the audit event `autoFailover`. `POST /failover/map/start` also takes `trigger` (default
`manual`) for the monitor. Cluster tests: `internal/clustertest/failover_auto_test.go`. Not tested in a cluster: two failures at once (needs five servers),
a rebalance running at the start, the stall tests of the design.

## The planned switchover (increment 7, round 1: mode graceful; contract 0.13.0 text only)

Code: `internal/failover/switchover.go` (the steps), `cmd/kingresd/switchover.go` (the callee: quiesce, unquiesce, mark, verify, resume, and the clean stop in `handleFailoverDemote` mode `fast`), `internal/instance/quiesce.go` (the hold rules), `Manager.Resume` in `internal/lease/adopt.go`.

Per group (the map group has the same steps, last, and its stop is described under "The map group with a live old leader"): `Freeze`, `Choose the standby` (a standby that streams from the primary; the verdict is none), `Stop the writes`, `Let distributed transactions end`, `Stop the old primary cleanly` (mark, the standby replays the mark, fast shutdown, the lease released), `Verify the new primary` (`switchover_verify`, repeated for 30 s), then `Wait for the old lease`, `Hand the lease`, `Promote` and the steps of every failover. Any failure before the promotion rolls back (`rollbackSwitchover`: the leases of the run are released, the old primary is resumed, the writes are let in again) and fails with `standby_behind`, `switchover_unproven`, `cannot_quiesce` or `prepared_transactions_pending`; a cancel does the same through `OnFail`; the cancel guard refuses from the start of `Promote`. The access rules of the old primary are put back in the last step, before its rebuild.

Deviations from the design: the old primary becomes a standby by the rebuild of increment 4 (`pg_rewind`, which finds nothing to rewind after a clean stop), not by the adoption `/standby/adopt {clean}` of 6.3; the phases `quiescing`, `waitingForTransactions`, `stopping`, `verifying` and `writesPausedSince` of `GET /failover` are not reported yet (the steps are named in the task, and the report has the events `quiesced`, `stoppedClean`, `verified` and `writesResumed` with `pauseMs`); `Unquiesce` is not persisted across a restart of kingresd (the failed task puts the rules back).

## The restore of the preferred primaries, the plan of a switchover and the phases (increment 7, round 2; contract 0.13.0 text only)

`POST /replication/restore-preferred-primaries` (optional body `{shardGroupIds, dryRun}`) starts the task `replication.restorePreferredPrimaries`: a switchover (the steps of
round 1) per group whose primary is not on its preferred server (the server of the group's first node by id), from the server that took it over; code in
`internal/failover/restore.go` and the per-group `Input.Olds`. Groups at home are not listed, a group whose preferred server has no ready standby and the map group are
reported `skipped`; a group that fails before its promotion is rolled back and reported `failed` while the task goes on and ends `partial`; 422 `unprocessable` when
nothing needs to be restored. `dryRun` answers the plan for a restore and for a switchover (`writesPausedSeconds`, the steps, verdict `none`). `GET /failover` names the phase of
a group of a switchover or restore from the task's steps and `writesPausedSince` from the events `quiesced` and `writesResumed`. See the design ("Increment 7, round 2") for
what is not built.

## The signing keys of the ledger (phase 7, steps 3 and 4, contract 0.11.0)

Design: `kingres/docs/design/ledger-phase7.md` (3.2 to 3.4, 6.7, 8.2 to 8.4, 9.1 to 9.3 and the corrections of section 14). This
increment is the control plane's foundation of phase 7: the contract (every operation of 8.4 is in `openapi.yaml`; the ones that
are not built answer 422 `unsupported_operation`), the migration, the scheduler with its term guard, and everything about the
**server signing keys**. What signs with them is the collector of the next section (steps 5 and 6).

### What exists

| Package or file | What it is |
|---|---|
| `internal/sysdb/migrations/0030_ledger_phase7.sql` | `kingres_cp.ledger_cluster` (singleton: cluster id, **term**, settings, key revision, published trust file, holders), `ledger_signing_key`, `ledger_event`, and `ledger_log` and `ledger_checkpoint` (written by step 5; read now to bound a retired key). The number 30 is a range of its own; the migration runner now applies every migration it has not recorded and allows gaps (see "Decisions") |
| `internal/scheduler` | the recurring jobs of the manager leader. On becoming leader it waits one `lease-margin`, increments the term in one statement and starts its jobs with a context that ends when leadership is lost. `Term.Guard` is the first statement of every write made under a term |
| `internal/ledgersign` | the daemon side of the keys: `Store` (PostgreSQL and memory), `Keyring` (the private key files), `Agent` (the internal calls), the tasks, the events, the trust file, the checks after every load, the jobs, and the operations of the REST API |
| `internal/api/ledgersigning.go`, `internal/httpapi/handlers_ledger_signing.go` | the seven operations that are real: `getLedgerSettings`, `updateLedgerSettings`, `getLedgerSigning`, `createLedgerSigningKey`, `retireLedgerSigningKey`, `revokeLedgerSigningKey`, `getLedgerTrustFile` |
| `cmd/kingresd/ledger_signing.go` | the wiring: flags, the scheduler, the calls, the alert |
| `internal/task/envfor.go` | `task.EnvFor`, an `Env` for calling one step in a test |

Settings (the pair `--x` / `KINGRES_X`, as the table above): `--ledger-keys-dir` / `KINGRES_LEDGER_KEYS_DIR` (default `<data-dir>/ledger/keys`;
the private keys are `signing/<fingerprint>.key`, mode 0600, written atomically by `internal/ledgerkeys`) and `--ledger-key-passphrase-file` /
`KINGRES_LEDGER_KEY_PASSPHRASE_FILE` (a file of mode 0600; without it the key files are in clear text, like `ca.key`, and `kingresd` logs one
warning at the start and `getLedgerSigning` says `passphraseProtected: false`). The passphrase file is read at the start (a wrong path or a
file others can read stops `kingresd`) and again whenever a key file is read or written; the passphrase is not kept in memory. Every holder
needs the same passphrase: a sealed file is opened with the holder's own passphrase and written again under its own sealing, and a holder that
cannot open it answers 409 `passphrase_mismatch` to the leader (the task says which server).

### The scheduler and the term (6.7)

`scheduler.Scheduler` follows the leader view of `internal/leader`. A server that becomes the leader waits `lease-margin`, looks again whether
it is still the leader, and runs `UPDATE kingres_cp.ledger_cluster SET term = term + 1 RETURNING term`. Its jobs get that term; `Term()` returns it
(and false for a follower, or while the margin runs, so a follower **never signs** and a key operation answers 503 `service_unavailable` with
`Retry-After`). **Every write of the key set is one transaction that begins with `SELECT 1 FROM kingres_cp.ledger_cluster WHERE term = $mine FOR
SHARE`** (`PGStore.Commit`): a deposed leader that wakes up finds another term, its write fails with `scheduler.ErrDeposed`, and a task step
that meets it ends with the task error code `term_over` (retryable). The share lock makes the next leader's increment wait for a write in
flight. The revision of the key set (`keys_revision`) is a compare and swap on the same statement, so two changes that read the same stored set
cannot both be written: the first wins, the other reads again (`ErrConflict`, tried five times). The task engine's own lease epoch guards the task
row; the term guards the key tables. The jobs of this step:

| Job | Every | Does |
|---|---|---|
| `ledger.key.due` | 1 min | `DueForRetirement`: starts the task `ledger.key.retire` (actor `system`, one audit entry) for an active key whose overlap ended |
| `ledger.signer.check` | 10 s | raises or clears the alert `ledgerSignerUnavailable` (critical, subject `cluster`): the key set is damaged, differs from the published trust file, or the file of an active or next key is missing on this server |
| `ledger.key.sync` | 1 min | compares what every holder has with what it should have and starts `ledger.key.distribute` only when there is something to do (an unreachable server that is a holder is tried again at the next pass; a server that left the cluster is forgotten; the same state is not started again for 15 minutes) |

### Tasks (all of subject `ledgerKey` / `signing`: one key task at a time, cluster-wide)

Every step reads the stored state again and does only what is not done; a repeated step writes nothing (a test runs every step of every kind
again and counts the commits). A step that must not go on says so with a task error code: `key_set_damaged`, `clock` (retryable), `term_over`
(retryable), `not_leader` (retryable), `last_signing_key`, `key_files_remain` (retryable), `key_file_missing`, `signer_unavailable`.

| Task | Steps |
|---|---|
| `ledger.key.create` (the first key) | Generate the key; Give the key to the servers that may become the leader; Publish the key-added event; Activate the key |
| `ledger.key.rotate` | the same, then Sign the latest checkpoint of every log again (recorded as `skipped` until step 5 provides the collector) |
| `ledger.key.retire` | Publish the key-retired event; Retire the key; Sign again; Delete the key files that no server needs |
| `ledger.key.revoke` | Publish the key-revoked event; Revoke the key; Sign again; Delete the key files that no server needs |
| `ledger.key.distribute` | Give the keys to the holders; Delete the key files that no server needs |

* **Generate.** The key is made with `ledgerkeys.GenerateKey`, checked with `Set.AddChecked` under the cluster origin and the origin of every
  log (a key with the key id of another is dropped before anything is written and another is made, up to eight times), its file is written, and
  then its row (state `next`, the id of the task, `mode` `file`). The first key also creates the cluster id (32 random bytes) and stores the
  origin prefix in force. A crash between the file and the row leaves a file with no row; the step that runs again **adopts** it (a file with no
  row, modified after the task was created less the allowed clock difference), and a key a cancelled task left in state `next` with its file here and
  no event is adopted too. A file older than the task is never taken: it may be the only copy of a key of a restored system database.
* **Distribute (3.2.2).** The holders are this server (the leader) and the servers that run an instance of shard group 0 (`kingres.node`,
  `group_id = 0`, `is_active`). The leader reads the key file as it holds it and sends it with the internal call `ledger/key/install` to a holder
  that lacks it (`ledger/key/list` says what a server has). A holder that cannot be reached does not fail the creation: the task detail names it, and
  the job `ledger.key.sync` gives it the key later. The recorded holders are the servers known to have every key, and a server stays on record until its
  files were removed.
* **Events (3.2.4).** Signed notes, key name = the origin prefix, numbered from 0 without gaps, one event of a type per key (a unique index), text
  as in `events.go`. `key-added` is signed by the active key (it authorises) and by the new key (it proves possession); the first key signs alone.
  `key-retired` and `key-revoked` are signed by another active key that is in force. Every signer is checked with `Set.CanSign` for the purpose.
  The `not-before` of `key-added` is the activation time the next step records, so the trust file says what the event says.
* **Retire.** The bound of the retired key (`max_tree_size`) is the largest size of any stored checkpoint and at least 1, written into the event and
  read back by the step that retires, so both agree after a crash. A key that signed nothing is bound to 1: the verifier then takes nothing of it
  that a thief could sign. **Revoke** needs another active key; the daemon never makes one inside a revocation. `compromised-since` is clamped
  to the time of the revocation and to the creation of the key (the library refuses both).
* **Times.** Every time given to the library is read by `Service.clock`, which compares the clock of this server with the database's and refuses
  (error `clock`, retryable) when they differ by more than two minutes; the time handed to Add, Activate, Retire and Revoke is the earlier of the clock and
  `Set.RecordLimit()`, so an idle set is brought forward in steps (a test rotates after two years of idleness). A user-given time (the
  `compromisedSince` of a revocation) is only ever the date written into the revoked key, never the time of a transition.
* **After every load** (`Service.load`): `Set.Validate()` (an error is the alarm of a damaged set, nothing is signed and no key task starts), and the
  trust file built from the set is compared with the one stored at the last change (`ledgerkeys.TrustFile` against `ledger_cluster.trust_file`); a
  changed bound, activation time, label or prefix is an alarm. `Service.Signers` returns `ErrNotLeader` without a term, `ErrNoKeys`, `ErrAlarm` for
  a damaged set (also for `ErrNoActiveKey`) or an unreadable key file, `ErrDoNotSign` for `ErrNotInForce` (a clock behind an activation: not an alarm),
  and `ErrClockSkew`.

### The internal calls (`ledger/key/*`)

Three calls, all `LeaderOnly` (internal-api.md, "The ledger keys"): `POST /ledger/key/install`, `/remove` and `/list`. The install body carries
the key file as the leader holds it (plain or sealed), never a passphrase; the answers carry fingerprints only. The task step's fencing epoch travels
with the call as for every leader-only call.

### Decisions where the design was silent or had to change (proposed corrections 121 onwards)

See the report of this step; the code carries them: the origin prefix is locked by the first key and not only by the first checkpoint (121);
the migration runner accepts gaps and applies every migration it has not recorded (122); `anchorLedgerNow` answers 201 (123); the subject
of the key tasks is `ledgerKey` / `signing` (124); `compromised-since` is clamped (125); a revocation never creates a key (126); holders are the
servers of group 0 (127); a retired key that signed nothing is bound to 1 (128); the signer for a rotation and the retirement must exclude each
other in step 5 (129); a key file is never taken from before the task (130); the passphrases of the holders must agree (131); the
trust file is built without the `next` keys (132); the tables carry columns the design's list lacks (133); the alert `ledgerSignerUnavailable` is also raised for a damagedset, a trust file that differs and a missing key file (134), a stale `next` key keeps its place (135), and `Content-Disposition` passes a forwarded answer (136). The numbers arethose of section 14 of the design (the first version of this paragraph numbered some of them differently). The review of the signing keys adds the decisions of the nextsection.

### Fixes for the review of the signing keys (P7-01 to P7-16)

The independent review found nothing critical or high. What was changed, and what is stated instead of changed:

* **P7-01, signing and retiring exclude each other.** One mechanism with three parts. (1) `Service.Publication` does not give the collector a key whose `key-retired` event is published. (2) The
  collector stores a checkpoint only while the revision of the key set is the one its signers were read at (`ledgerpub.PutCheckpoint`), and every change of the key set, an event included, raises
  the revision. (3) Both commits of a retirement carry `Change.CheckpointBound`: the store, holding the row of the cluster under the term guard, refuses the change when a checkpoint above the bound is stored,
  and the retire step reads the bound again and ends with the task error `bound_exceeded` (the key stays active) when a checkpoint above the bound of its event was stored. Tested with a retirement and a
  signing pass interleaved at every boundary of the engine (`review_test.go`).
* **P7-02, the clock.** The "second source" of the clock is the database of the same host on a one-server cluster, so the comparison proves nothing there: a clock that is wrong by an hour passes. It is stated
  here and not fixed: with several servers the operator is warned by the existing alert `clockDifference` (which compares servers), and an operator of a one-server cluster must keep the clock right (NTP) before the
  first key is created and before a rotation: a first key activated in the future cannot sign until that time, and `ledger.key.due` retires a key early when the clock jumps forward.
* **P7-03, a server that left.** A server that is no longer a member (`kingres_cp.server`) and cannot be reached is forgotten as a holder: it leaves `Cluster.Holders`, the log says that key files may remain on
  that host and that the operator must wipe it (the daemon cannot), and the distribution is not started again for it. A server that is still a member and down stays a holder and is tried again. The sync job does not
  start a distribution again for the same state within 15 minutes (a holder with another passphrase, or a host that never answers, would otherwise make a task row a minute). A revocation whose last step could not
  remove a file from a reachable-later server is run again by calling `revokeLedgerSigningKey` once more while a server still has a file of that key (it is refused as before when none has). What remains: the task rows
  that finished are not pruned (there is no retention in the task engine; the task of the job is started rarely now), and the removal of a server does not wipe its key directory by itself (the sync job does it while the server
  answers and is no holder; a server that goes away before that must be wiped by hand).
* **P7-04, a key file that does not open.** `alarmText` opens the file of every active and next key and compares its public key with the row: garbage, another passphrase and another key under the name are the alarm
  (`signerAlarm`, the alert `ledgerSignerUnavailable`). A holder answers `ledger/key/list` with the files that do not open (`unopenable`); the distribution installs the key again over them (`Keyring.Install` replaces
  a file that does not open, never one that holds another key).
* **P7-05, the stored state.** At every load the chain of events is verified: numbered from 0 without a gap, each note parses, carries the number and type of its row, and is signed by a key of the stored set. A changed,
  added or deleted event is an alarm (no signing, no trust file is served). What an attacker who can write to the system database and holds no signing key can still do: change key rows together with the stored trust file
  (the two live in the same database), which makes the daemon publish a trust file that lists their key; they cannot make it sign (no key file), cannot forge an event, and an auditor who obtained the fingerprint out of band
  does not trust the key. Events deleted from the end are not seen. The trust file of the API is no source of first trust; an operator who needs more keeps a copy of `getLedgerTrustFile` off the cluster.
* **P7-06**: the `compromised-since` of a revocation is decided once, in the event, and the revoke step reads it back. **P7-07**: `kingresd` sets `RLIMIT_CORE` to 0 and is not dumpable (`harden_linux.go`); the install body
  and the base64 text of a seed on the install path are strings that Go does not let us zero: they stay in memory until the garbage collector frees them, and that is documented, not removed. **P7-08**: errors of key files
  name the fingerprint, never a path. **P7-09**: temporary key files older than a minute are removed when the keyring starts and when the list call is answered. **P7-10**: a key file is adopted only when it carries
  the marker the daemon writes next to it (`<fingerprint>.task`, written before the file); a file that somebody placed has none. The marker is no boundary against somebody who can write the directory (they can write it too); the passphrase seal is.
* **P7-11**: a due retirement that was started is not started again for 10 minutes. No alert at 14 keys and none for a revocation (the audit entry is the record): not added. **P7-12**: the set is read again immediately before a
  key file is removed, and the file of a key made since the first read is never removed. **P7-13** (a leader change between "generate" and "install"): not changed; the task fails with `key_file_missing` and the key can be revoked
  once another key is active. **P7-14**: the README numbers now follow section 14; the contract text of `originPrefixLocked` still says "a checkpoint exists" while the server locks at the first key (correction 121): it
  needs a documentation change of the contract at the next revision. **P7-15**: unchanged (noted).
* **P7-16, the migration runner.** Decision: a migration with a number below one already applied is applied late, after the higher ones, and the rule is that such a migration must not alter what a higher one
  relies on (migrations 15 to 29 are reserved for the failover work and are independent of 30 and 31, which only create tables and add columns of their own). The test `TestAMigrationInAGapIsAppliedLater` of
  `internal/sysdb` states the behaviour.

### What steps 5 and 6 will find

* `Service.Signers(ctx)` gives the opened signers (all of them during an overlap: sign every checkpoint with every one) and refuses with the
  errors above; `Service.Set(ctx)` gives the validated set for `ledgerkeys.TrustEntries` and `ledgerlog.ResignCheckpoint`.
* `Config.Resign` is the hook the rotation, retirement and revocation tasks call (steps "Sign the latest checkpoint of every log again"): step 5 sets it.
* `Store.LogOrigins` and `Store.MaxCheckpointSize` read `ledger_log` and `ledger_checkpoint`, which step 5 fills; **the collector must write a checkpoint in
  a transaction that begins with `Term.Guard`**, and it must **not sign with a key the retirement is retiring**: a checkpoint signed by a key after the size
  was chosen for its bound is refused by the verifier. The retirement reads `MaxCheckpointSize` in its first step; step 5 has to make signing and that
  read exclude each other (for example by taking the same revision check, or an advisory lock) or to sign only with the keys of the set it read in the same transaction.
* The scheduler is built to take more jobs: `Scheduler.Add(Job{Name, Every, Jitter, Run})`; `ledger.round`, `ledger.digest` and the others are added in `newLedgerParts`.
* `ledger_event` holds the notes the publication serves as `events/<seq>`; `ledger_cluster.trust_file` is the file the keyring will serve.

### Tests

`internal/scheduler` (leader, margin, term, loss of leadership, deposition, panics), `internal/ledgersign` (state machine as the daemon drives it, events,
idempotence of every step, alarms for every kind of damage, clock, idle set, term guard, follower, distribution and its failures, passphrases, secrets
searched in every output, crash points of every task kind at every engine fault point, a kill inside a step), the same on the database store with
`PGStore` (`TestTheKeyLifecycleOnTheDatabaseStore`, `TestADeposedWriterCannotWriteOnTheDatabaseStore`, `TestTwoChangesFromOneStoredStateCannotBothBeWritten`,
`TestTheDatabaseStoreKeepsWhatItIsGiven`), `internal/httpapi` (`TestLedgerSigningOperationsMatchTheContractAndTheRoles`), `internal/sysdb` (gaps), and the
cluster test `TestLedgerSigningKeysAreMadeDistributedRotatedAndRevokedAcrossTwoServers` (two servers, standbys, forwarding, files on both, events,
trust file read by the verifier, no seed in any log, row, task or audit entry, a killed and restarted leader, a deposed leader).
**(HA)**: a leader change needs the failover of the map group, which answers `unsupported_operation` in this revision (`internal/failover/steps.go`); the
test of a key available after a leader change is therefore not written. What stands in for it: the second server holds a key file that opens and has
the public key the API shows (the cluster test), the scheduler restarts its loop and takes a new term after a kill and a restart of the leader (the
cluster test), and a new service over the same store and key files signs (`Signers` after `rebuild` in `TestTheKeyLifecycleOnTheDatabaseStore`): nothing but the
store, the key files and the clock is needed. The test of a real leader change is to be written with the failover of the map group.

## The collector, the chain logs, the aggregate and the publication (phase 7, steps 5 and 6, contract 0.11.0)

Design: `kingres/docs/design/ledger-phase7.md` (4.1 to 4.7, 5, 6.7, 8.3, 9.4 and the corrections of section 14, with the decisions below). The contract is
unchanged (0.11.0): `getLedgerPublication`, `getLedgerAggregate` and `listLedgerAggregates` stop answering `unsupported_operation`, `generateLedgerDigest`
reads every shard group, `LedgerDigest.aggregateHash` and `aggregateSize` are filled.

| Package or file | What it is |
|---|---|
| `internal/sysdb/migrations/0031_ledger_publication.sql` | `ledger_tile` (write once: the primary key refuses a second row, a trigger refuses an update), the columns a log needs beyond migration 30 (why it stopped, the aggregate leaf that closed it, the aggregate leaf that is being appended, when its group became unreadable), the unique indexes (one open log per chain, one aggregate log per database name) and `ledger_digest_view` |
| `internal/ledgerpub` | the `Store` of the publication (`PGStore` in `kingres_cp`, `MemStore` for tests, both under the same test suite), the parser of the publication paths, the HTTP `Handler` and the `Listener` (plain or TLS) |
| `internal/ledgerround` | the collector: `Reader` (the callee of `ledger/read`), `Source` (the groups and the calls), the round (`chain.go`, `extend.go`, `aggregate.go`), the retirements (`retire.go`), the digest and the views of the API (`digest.go`, `views.go`), the re-signing at a rotation (`resign.go`), the files the listener serves (`objects.go`), the jobs (`jobs.go`) |
| `internal/ledgersign/publish.go` | what the collector takes from the keys: `Publication` (the settings, the signers, the revision of the key set), `AppendEvent` (the `chain-retired` event), `Prefix` |
| `cmd/kingresd/ledger_signing.go` | the wiring: the collector, the listener, the internal calls, the hooks of `dropDatabase` and of the removal of a server |

Settings (the pair `--x` / `KINGRES_X`): `--ledger-publication-listen` / `KINGRES_LEDGER_PUBLICATION_LISTEN` (`host:port` of the listener that serves the published
files; empty, the default: no listener), `--ledger-publication-tls-cert` and `--ledger-publication-tls-key` (together; without them the listener is plain HTTP and the
console warns). The listener serves `GET` and `HEAD` for the layout of design 4.6 and nothing else, with no authentication (the files are public by design), no
cookie, no redirect and no listing; a path that is not in the layout, or that needs decoding, is a 404 with `max-age=5`. Tiles, bundles and events have
`Cache-Control: public, max-age=31536000, immutable`; the checkpoint and `keyring.json` have `max-age=5`. At most 200 requests are served at once (more get 503) and
50 per second per client address (more get 429). Every server can have a listener: a follower asks the leader (`ledger/publication/get`) and keeps the immutable files in
a bounded cache. `keyring.json` is the trust file of `getLedgerTrustFile`. The listener serves whatever is stored while it runs, also when the setting
`publication.enabled` is off (the setting stops the collector, not the files that were published).

### A round (5.4)

The job `ledger.round` wakes every 10 seconds on the manager leader and runs a round for every database with a ledger every `publication.intervalSeconds`
(`ledger.digest` runs one per `digestIntervalSeconds`, with the operation that closes the open block and records a digest). A round reads the head of each shard group
from the server that runs its primary (`ledger/read`, 10 seconds), appends the new blocks to the chain log of that group (at most 9 999 per round; the first block read is
the last published one and its hash must still be the published one), and then builds the aggregate leaf from the logs. The order of every log is the one of `ledgerlog`:
append the tiles, store the frontier with its size and root, sign, store the checkpoint; each is a write of its own whose first statement is the term guard. A log is opened
with its stored frontier; when tiles are ahead of it (a kill between the tiles and the frontier) it is opened once with `AllowTilesAhead` and the same leaves are appended again,
and another leaf is a divergence. A stored tree without a checkpoint (a kill between the frontier and the checkpoint) is signed at the next round. The leaf of the aggregate holds
the time of the round, so it is stored in the log row before it is appended (`PutPending`), and a crash appends the same bytes. A round that finds nothing new writes nothing.

* **Never a smaller tree, never a second root.** `PutCheckpoint` takes the log row for update and refuses (`ErrConflict`) a note that is not for the stored size and root, a
  checkpoint below one that was signed, another root for a size, and a log that is not active; the signature is made over `Log.Checkpoint()` only.
* **Signing and retiring a key exclude each other (corrections 128 and 129).** `Publication` returns the revision of the key set the signers were read at, and `PutCheckpoint`
  stores a note only while the revision is still that one (`ErrKeysChanged`: nothing is stored, the next round signs again). The retirement states its bound inside the change
  that records the event, and the store refuses that change (`Change.CheckpointBound`) when a checkpoint above the bound was stored after the bound was read; the step reads the
  bound again. A key whose `key-retired` event is published is no longer given to the collector.
* **Divergence (4.7).** A head below the published size, a head block that is not the published block at its height, a first block read that is not the last published one, an entry that
  is not a header, blocks that do not follow each other, or tiles that differ from the leaves: the log is stopped (`diverged`, with the reason), the alert `ledgerChainDiverged`
  (critical, `database:<name>|group:<n>`) is raised, no tile or checkpoint is written for it and nothing repairs it; a chain that comes back to the published history does not wake the
  log up. Its member in the aggregate is flagged `diverged` with the last good head. The other chains go on.
* **An unreachable group.** A group that cannot be read, answers an error, or has no ledger or no database is not covered: its members are `stale` (the last good head repeated), the
  digest says `complete: false` and names it, and an alert `ledgerGroupUnread` is raised (warning after a minute, critical after an hour). It is never skipped as complete.
* **A follower never signs and writes nothing**: a round on a server that holds no term is `ErrNotLeader` before the first write; a leader whose term ended gets `ErrDeposed` at its
  first write.
* **Re-signing at a rotation.** The hook of the key tasks (`Config.Resign`) adds the signature of the keys that sign now to the latest checkpoint of every log, growing or not, closed or
  not, after the note was verified under every key of the trust file (`ledgerlog.ResignCheckpoint`); only the signers that have no line yet are added.

### The aggregate (5.2)

One aggregate log per database name (origin `<prefix>/a/<id>`, the id from the cluster id and the name). A leaf is appended only when the members, their heights, hashes, roots or flags
differ from the previous leaf. A database with more than 512 chains is refused (`ErrTooManyChains`; the aggregate does not move). `getLedgerAggregate` reads the leaf from the tiles after
checking them against the root of the checkpoint (a damaged store is a 503, never a wrong answer); `complete` and `uncoveredShardGroupIds` are the groups whose member is stale or diverged.
`generateLedgerDigest` runs a full round and returns the digest of every group read, stored as a view; `listLedgerDigests` lists the views (the digests that the extension recorded before this version
stay in each database and are not listed).

### The end of a chain

* `dropDatabase` begins with a step that runs a final round, publishes the event `chain-retired` for every chain (`chain`, `epoch`, `final-height`, `final-block-hash`, `reason: dropped`), closes the
  logs and appends one aggregate leaf in which every member is flagged `retired`. The aggregate log stays open: a database of the same name continues it with new chains.
* The removal of a server does the same for the chains of the groups whose primary runs on it (`reason: group-removed`), before anything is changed on the server.
* **A database that disappears without `dropDatabase`.** The owner of a database is a role that is not a superuser, and it can drop the database from SQL; nothing of the control plane sees it. When
  every group has answered "no database" without one other answer in between for 30 minutes (`KINGRES_LEDGER_MISSING_FOR`, a duration, changes it; time, not a count of rounds), the system instance does not have the database either, and a critical alert (`ledgerGroupUnread`, subject the database) has warned from the first answer on, the collector ends the publication of its chains with the same event and `reason: database-missing`, one aggregate leaf
  with the members flagged `retired`, and closed logs. An auditor tells it from a rollback: a rollback is `diverged`, with an alert, and the chain log stops with the old head; this is `retired`,
  with a signed event and no alert of divergence. One group that answers "no database" while the others answer is only an unread group. A group that holds another chain than the one published
  (a database made again outside the control plane) gets the same end for the old chain (`reason: chain-replaced`) and a new log for the new one. Options that were rejected: a revoke of the
  right to drop (it would change who owns existing databases); an event trigger (`DROP DATABASE` does not fire one, and it would run in the database that is being dropped); treating the missing
  database as a rollback (an auditor could not tell a deletion from a restore); doing nothing (a log that is stale for ever, with no record of why). The event states the last published head, not a
  head read at the end, because the database is gone.

### What a test of this step runs

`internal/ledgerround` (memory store and fake source: the sizes of the verifier, rounds, a cut at every write of a round and of a retirement, divergences, unreachable and back, retirements,
digests, views, re-signing, a follower and a deposed leader, 512 chains; and the same rounds on PostgreSQL), `internal/ledgerpub` (the rules of the store on the memory store and on PostgreSQL, the HTTP
behaviour, a TLS listener), `internal/ledgersign` (the signers, the exclusion with the retirement, the event), `internal/httpapi` (the three operations against the contract), and in
`internal/clustertest` two servers with a database on two groups: the digest, the aggregate, the files over both listeners, the verifier, a kill at each kind of write, the cut off group, the drop and the
vanished database.

### What the review of the collector and the publication changed (corrections 171 to 178)

* **What was published is kept outside the system database (P8-01).** `<ledger-keys-dir>/published.json` holds the highest size and root of every log that has a signed checkpoint, the number of events and the
  hash of the last one (package `ledgerstate`; mode 0600, written atomically, with a checksum; no secret). The aggregate leaf and the events hold a clock time and what the chains looked like then, so they are not
  reproducible, and a restore of `kingres_cp` from an older backup made the publisher sign a second root for a size and a second event for a number with no alert. Now every round, every retirement and every key task that
  appends an event begins by comparing the database with the record: a database that holds less than was published, or another root at a published size, or another last event, refuses the round (nothing is signed or
  written) and raises the critical alert `ledgerSignerUnavailable` with the subject `published-state`; it clears itself when the database holds what was published, or when an operator accepts the loss (see "The review of the wiring of anchoring and its fixes", restore of the system database). Before a checkpoint is signed the record is asked
  again (`Allow`), so a size that was published is never signed with other content whatever the database holds. The record is updated after the store accepted a checkpoint or an event; a crash in between leaves
  it one behind the database, which is harmless. It is not signed: whoever can write the key directory can already sign with the keys. A **damaged** file refuses everything until an operator removes it.
* **Every holder.** The leader sends its record to the holders at every pass of the job `ledger.state.sync` (internal call `ledger/state/merge`: the larger size per log, the larger event count; two copies that
  disagree on one size are refused), and a server that is about to lead and has none asks the others first (`ledger/state/get`) and merges what they hold. Only when no holder answers does it take what its
  database holds as the starting point, and a log line says so: this is also the first start after an upgrade from a tree without the record. **What is not covered:** a restore of the database when every copy of the
  record is lost or older than the publication; a mirror or an auditor that holds the later checkpoint is what shows that fork. A restore of both the database and the record to the same older point is the same case.
* **A retirement is about the checkpoints of the key that retires (P8-02).** After the event `key-retired` the other key keeps signing and the logs grow; the check at the retirement asks for checkpoints that carry a
  signature of the retiring key above its bound, not for the largest tree of the cluster, and the task ends.
* **A database that vanished (P8-03).** Time (30 minutes), the system instance and a warning alert instead of two rounds; a chain whose log was closed (any reason) and that comes back with the same chain id is not
  opened again and not published in silence: a critical `ledgerChainDiverged` names the group, the height and why the log ended, and its new blocks wait for the new epoch of step 20. An auditor sees the closed
  log with its `chain-retired` event and nothing after it.
* **Swapped answers and replaced chains (P8-04).** An answer that names the chain of another group's open log is a divergence of both logs (critical alerts, nothing closed, nothing signed). A chain that is really
  replaced still ends with a signed event and starts a new log, and now raises a critical alert as well.
* **The rate table (P8-05)** evicts the entry that asked longest ago when it is full, and an IPv6 address counts by its /64. **`ReplaceNote` (P8-06)** accepts only a note whose text is the stored checkpoint's and
  only while the key revision is the one the signers were read at. **A diverged log that is retired (P8-08)** stays flagged `diverged` in its retiring leaf and its alert is not cleared. **The adoption marker (P8-07)**
  reads `task <n> key <fingerprint>`, and `Orphan` takes the id of the asking task (a file marked by a later task is not adopted); the marker cannot be made unforgeable against a person who can write the key
  directory, and the text says so.
* Decided, not changed (P8-10): tiles and events stay `immutable` (the record now makes a re-signed size impossible, so a mirror's copy cannot be contradicted by this publisher); the aggregate id is a hash of the
  cluster id and the database name, hidden only as far as the name is unguessable (design 4.9 says so); the listener reads its certificate at start; a peer's answer over the asked number of blocks is refused by `Append`
  (10 000) and bounded by the 64 MiB limit of the peer client; a future hash version of blocks stops the logs with a divergence that says "not a block header", and needs the decision of step 20 before a version 2 exists.

## Anchoring of the aggregate checkpoints (phase 7, step 10; the WORM target; contract 0.11.0)

`internal/ledgeranchor` wires the libraries `internal/anchor`, `anchor/sigv4` and `anchor/worm` into the daemon. What it does, by the sections of design 6, with the decisions of corrections 179 to 183:

* **Targets and credentials.** A target is a row of `kingres_cp.ledger_anchor_target` (migration 0032): `kind`, `name`, `enabled`, the non-secret configuration in the spelling of the library of the kind (stored normalised, the
  defaults filled in), `credentialsSet`, the last test. The credentials are write-only: `createLedgerAnchorTarget` and `updateLedgerAnchorTarget` take a `credentials` object (WORM: `accessKeyId`, `secretAccessKey`,
  optional `sessionToken`; at least four printable ASCII characters each, so that the redaction can find them), the service validates it, writes `<ledger-keys-dir>/anchor/<id>.json` (mode 0600, in clear without a
  passphrase file, sealed with AES-256-GCM under PBKDF2 of the passphrase file with one; written atomically) and hands the file to the holders by the internal call `ledger/anchor/credentials/install`. Nothing returns
  them: not the target, not the list, not an error, not a log line, not a task row, not an alert, not the audit entry (it says `credentialsChanged`, no more). `{}` removes them. A target may instead name
  `credentialsPath` in its configuration, the operator's own file (JSON with the same members, a regular file that only its owner can read), read at every use and never copied. An enabled target needs credentials.
  `credentialsSet` is false until they are given, and a target without them is built as a failed check, not as a crash.
* **Records and jobs.** `kingres_cp.ledger_anchor` keeps one record per (target, aggregate log, size) with the signed note it was made for (a re-signing at a rotation does not change the bytes a retry sends), the evidence as
  text (the strict parsers read canonical bytes only), the counters of the library, `submit_start` and `generation`, and a `version` that every write raises. Four jobs run on the leader under its term:
  `ledger.anchor.enqueue` (every `anchor.intervalSeconds`, and on `anchorLedgerNow`: a record of the newest checkpoint per enabled target and database; an older record that is still pending is superseded in the same
  transaction), `ledger.anchor.submit` (30 s), `ledger.anchor.upgrade` (60 s) and `ledger.anchor.monitor` (10 s). Every write begins with the term guard; a follower holds no term and runs no job, a deposed leader's write is
  refused. A submit is made in three writes that survive a kill at any point: the start of the first submission is written before the call (duty 1 of the library), the call is made with no transaction open and a
  timeout, and the result is written by a compare and swap on the version of the row (a result that finds the row changed is dropped). A target that hangs holds one place of a pass, not the pass.
* **What anchored means.** Only a receipt that the library of the kind returned, with evidence that its own parser reads back, moves a record to `anchored`; a record's state changes only through the methods of
  `anchor.Record`; `claimed` (OpenTimestamps) is shown as `submitted` and never counts. `retryLedgerAnchor` puts a failed or expired record back to pending (it never raises the generation of a WORM key, which is an explicit
  choice the contract does not offer; the start and the generation are kept). `downloadLedgerAnchor` writes the evidence file of the library (`json`); `ots` and `tsr` come with their targets.
* **Outgoing requests (correction 180).** A client reaches exactly the host and port of the configuration (the endpoint, or the S3 host of the region with the virtual host of the bucket) and no other: the dialer refuses
  every other address, the libraries follow no redirect, and no proxy is read. An address written in the configuration is connected to as written; a host name that resolves to a loopback, link-local, unspecified or
  multicast address is refused (a name that leads to a metadata service or to this server); private addresses are fine for names. Without `caBundlePath` the roots of the system are used; with one, those only.
* **Alerts (6.6, 6.9).** The alerts are a function of the state, recomputed every 10 s and written only when one appears, changes severity or goes away: `ledgerNotAnchored` (`database:<name>`, info, warning when
  `anchor.required`), `ledgerAnchorBehind` (`target:<id>`: warning after three intervals and an hour, critical after twelve and an hour, counted from the oldest checkpoint the target has not covered and not earlier
  than the target was switched on), `ledgerAnchorFailed` (`target:<id>`, from a failed or expired record, worded from the code and the text of the error, cleared by a Retry or by deleting the target) and
  `ledgerAnchorConflict` (`database:<name>|target:<id>`).
* The settings are those of `getLedgerSettings`; the contract did not change (the operations were in 0.11.0 and answered `unsupported_operation`).

### `logs.json`, the whole fixture and the blocks of a quiet database (correction 178)

* **`logs.json`** is served at the root of the publication next to `keyring.json` (short cache, `application/json`): `{"format":1,"logs":[{"kind":"chain|aggregate","origin":"...","state":"active|diverged|closed","size":N},...]}`,
  one line and a newline, the members in this order, the logs in the order of their creation, a log with no checkpoint yet included. It holds no database name (an origin is made of the chain id or of the aggregate id).
* **What changed in the bytes that the verifier reads, from the fixes of the review:** the tiles, the checkpoints, the entries of the chain logs, the aggregate leaf and the text of an event are unchanged byte for byte.
  Two things differ. The leaf in which a diverged chain is retired by a drop or a removal now carries the flags of the member as `retired` and `diverged` together (the flag byte 0x06, where it was 0x02), and the event
  `chain-retired` for it is unchanged. A chain whose log was closed and that comes back is never reopened and never gets a leaf, an event or a tile (an alert only), so no number of an event and no size of a log is ever given
  to two texts: that assumption of the verifier is now kept by the record `published.json` and by the checks above, with the limit stated there.
* **The fixture of the cluster test** (`KINGRES_PUBLICATION_FIXTURES`) still saves checkpoints and tiles of the latest state only, and no `events/`, `keyring.json` or `logs.json`. Making `TestLedgerPublicationOfADatabaseOnTwoGroups`
  keep the whole publication (these files, and two successive checkpoints of each log by saving twice with a round in between) needs a cluster run of the test on both majors, which this round could not queue; it is the first thing
  to do before the verifier's writer regenerates `kingres/verifier/testdata/publication`. The 305 blocks of a chain where the test wrote 5 rows are not explained yet: the builder's schedule, a block per digest and the
  blocks of the extension's own tables are the candidates, and whether a quiet database grows by itself is to be measured on a cluster (the number of blocks after an idle hour).

### The RFC 3161 target (phase 7, step 12; correction 184)

`kind_tsa.go` registers the kind `tsa` over `internal/anchor/tsa`. The configuration is the library's: `url` (http or https, no user information, no query), `caBundlePath` (required: the trust anchors and intermediates that the **token** is checked against; it must hold a
self-signed certificate), `policyOid`, `hashAlgorithm` (`sha256`), `timeoutSeconds`, `maxSkewSeconds`, `signerKeySha256` (the pin of one authority) and `allowInsecure` (needed to send the Basic credentials over http). The TLS connection to
the authority is checked against the roots of the system, not against that bundle (the chain of a token and the certificate of the server are two matters). The credentials are an optional HTTP Basic pair (`username` and `password` go together). A
record is `anchored` at once on a token the library verified in full (message imprint, nonce, chain at `genTime` and now, key usage, names); every refusal of 6.4 is a failed record with the code of the library in the alert. Requests go to the
host and port of `url` only (the egress rule). `downloadLedgerAnchor?format=tsr` gives the DER of the time-stamp token (the CMS ContentInfo from the evidence), not a whole TimeStampResp, which the cluster does not keep. The Test button
asks for a real token for the hash of an empty string and verifies it.

### The OpenTimestamps target (phase 7, step 14; corrections 185 and 186)

`kind_ots.go` registers the kind `ots` over `internal/anchor/ots`: the library's configuration (`calendars`, `minCalendars`, `upgradeTimeoutDays`, `allowInsecure`, `contradictionConfirmMinutes`) and one member of ours, `headerSource: {"url": ...}`, an
Esplora-compatible explorer of a node the operator trusts (`/block-height/<h>`, `/block/<hash>/header`; the answers are bounded and the hash named must be the hash of the header given). Without it a claim of a calendar is a **claim**: the record is
`claimed`, shown as `submitted`, never counted as anchored, looked at again at its next time; with it a record is anchored only on the header of the claimed height passing the library's rules. The calendars and the header source are the hosts the egress rule
allows. No credentials. `jobs.go` gives every submitted or claimed record a time for its next look (`UpgradeInterval`), also a claimed record that has none, applies `ExpireIfDue` with the timeouts of the library, keeps what the calendars that answered gave when a
submission is partial and passes it to `Resume` at the next attempt, and notes contradicted claims in `lastError` and the log. The Test button warns when the table of real headers of the build is older than a year, and so does a line in the log at start.
`downloadLedgerAnchor?format=ots` writes the detached `.ots` file.

### Part A of the last round (corrections 187 to 192)

* **The 305 blocks (187):** explained, not a fault. The helper of the cluster test makes one digest for each row it writes, and a digest closes a block, so the rows of a test make as many blocks as the test has digests plus the
  blocks of the extension's own tables. `TestAQuietLedgerDatabaseStaysQuiet` (`KINGRES_QUIET_MINUTES`, default 3) writes five rows with no digest, lets the collector publish, and then watches ten minutes: the blocks, the digests,
  the sizes of every published log and the number of events did not change (pg18: blocks `[1 1]` to `[1 1]`, digests `[0 0]` to `[0 0]`, 659 s). The builder puts the pending transactions of one visit into one block.
* **The whole fixture (188):** `TestLedgerPublicationOfADatabaseOnTwoGroups` saves the whole publication (`events/`, `keyring.json`, `logs.json`, the tiles and the latest checkpoint of every log) twice: `first` after the first
  verifier pass and `second` after growth and a round, so every log has two checkpoints; one WORM target (the fake S3 of the test) holds one anchored record and its evidence file. To regenerate:
  `scripts/run-integration.ps1 -Fixtures <windows dir> -Run TestLedgerPublicationOfADatabaseOnTwoGroups` (with `-Install` for the other major) for each major; the directories are `<dir>/first` and `<dir>/second`. `-TestEnv 'A=b;C=d'` passes environment to the test.
  Nothing under `kingres/verifier` was changed.
* **Process kills in a round (189):** `TestLedgerSurvivesAKillAtEveryOtherKindOfWrite` kills the leader at a write of kind digest, log, unread, state, retired, event and note (the hook `fault.Spec.LedgerExitKind`; "event" is new, at the
  commit of a key event in `ledgersign.PGStore`), restarts it, and checks: no log diverged or closed that should not be, the stored logs audit against their tiles, every log passes the verifier, and `published.json` agrees with the database.
  `TestTheConcurrencyLimitIsNeverExceeded` (package `ledgerpub`) holds the limit of two with ten requests: the peak is two, eight get 503 with `Retry-After`.
  `TestAFollowerThatForwardsAHostileBodyLeaksNoCredential` sends truncated, oversized, mistyped and trailing bodies with credentials through a follower: no answer, log or audit entry holds a credential.
* **Duties 9 and 10 (190):** the Test of a WORM target reports the checks `retention` (ok), `retention-ends` (warning when the object lock ends within 30 days), `retention-ended` (failed), `evidence-attempt` (warning when `attempt` is null),
  `evidence-found` (warning when the store says the evidence was found) and `server-clock` (the store's `Date` against the database clock: warning above 30 s, failed above 5 minutes, the text names both times). The same checks run in the
  monitor job and as an hourly line in the log.
* **Retry limits (191):** the contract has no member for them, so the daemon reads `KINGRES_ANCHOR_MAX_RETRY_ATTEMPTS` and `KINGRES_ANCHOR_MAX_RETRY_AGE_HOURS` (the names the settings would have: `anchor.maxRetryAttempts`,
  `anchor.maxRetryAgeHours`); a bad value is refused at start with a log line and the default is kept.
* **Evidence files in the publication (192):** `a/<aggregate id>/anchors/<size>/<target>.json` serves the canonical evidence of an anchored (or submitted or claimed, with evidence) record, only at that path; it is never listed
  (`logs.json` is unchanged) and is `Mutable`, so a short cache.
* **Found by the cluster runs:** a drop task ends the publication first and drops the database a few steps later, so the collector saw the chain of a database it had just retired as a closed chain coming back and raised a critical
  alert. For the reason "dropped" that is now silent for `DropGrace` (30 minutes) after the retirement. The step "Drop the database" also tries the drop again on SQLSTATE 55006 (a session that connected between the termination of
  the sessions and the drop).
* **"Anchored" through the header source (not done):** the OTS library has no exported way to inject the proof-of-work check, and a real block header cannot be matched to the merkle root a calendar returns, so the outcome
  "anchored" through the wiring cannot be shown without a library change. The claim path is tested up to `claimed`.

### Client keys, the upgrade, bundles and the audit (phase 7, steps 17 and 20; corrections 207 to 211)

* **Client keys, signature policy, upgrade (step 17).** Every shard group has its own registry (the tables of `kingres_ledger` 0.3 are ledger tables of that group's chain), so `registerLedgerClientKey`,
  `revokeLedgerClientKey` and `setTableSignaturePolicy` run on every server through the apply kind `ledger.client`: a check here and on every reachable server first (a group on 0.2 refuses the whole operation and is
  named), then the change here, then on the other servers; an unreachable server gets the task `replicate.apply`. Every apply is idempotent. `upgradeLedgerExtension` starts the task `ledger.upgrade` (subject
  `database/<name>`, one step for this server and one for each other); 0.3 is detected by object (`kingres.ledger_client_key`), not by number. The API names a key by the first 8 hexadecimal characters of its 16-byte
  id, and refuses a registration whose short id belongs to another key. Who registered and who revoked is in `kingres_cp.ledger_client_key_actor` (migration 0033), because the extension records only the service role.
* **A group added later (correction 211).** When the log of a chain is created for the first time, the collector calls `Ops.SyncClientKeys`, which copies the registry of this server's group (the manager leader's) to
  the other servers, revocations included (idempotent). The policies are not copied: a ledger table lives on one group, so a new group has none. Limit: it runs in the publication's round, so a cluster whose
  publication is off is not synced until it is on.
* **Bundles (step 20).** `createLedgerBundle` gives the bundle a number (migration 0034), starts the task `ledger.bundle.create` and writes `bundle N` in its detail (what the console reads). The task writes
  `<data dir>/ledger/bundles/bundle-N.tar.gz` (the layout of the publication restricted to the database's logs, the events, the key ring, `logs.json`, the evidence files with `includeAnchors`, the proofs asked for as
  `proofs/proof-N.json`, and `bundle.json`). `getLedgerBundle` is answered by the manager leader (which built it), 404 for another database's bundle, 410 after 24 hours (the file is removed then). A bundle is lost
  with a change of the leader (the record names the server).
* **The audit of the publication.** `startLedgerVerification` accepts `checks`: `chain` (always run: the task leaves the extension's record), `publication` (every tile and entry bundle of the database's stored logs against
  the root of the latest checkpoint, and the stored note against the stored size and root; the findings are in the error of the task) and `anchors` (422 `unsupported_operation`: not built). The per-check member of
  `LedgerVerification` is a proposal for 0.13.0 (`p7c-work/contract-0.13.0-ledger.yaml`); until it is in the contract the outcome is the state of the task and its error.
* **The new epoch (step 20).** `acknowledgeLedgerDivergence` (400 for a wrong `confirm`, kind or reason; 409 `chain_not_diverged`) starts the task `ledger.epoch.start`: `Collector.StartEpoch` signs and publishes the event `epoch-closed` (lines `chain`, `epoch`, `final-size`, `final-root`, `common-height`, `kind`, `reason`, `acknowledged-by`), closes the old log (kept and served, detail `retired: epoch-closed (was diverged: ...)`), creates the log of epoch e+1 empty (same chain id, origin `./e<e+1>`), clears the alert and appends the aggregate leaf that flags the old member retired and diverged; the next round builds the new log from block 0 and the next leaf names it. Every step is idempotent and resumable. `common-height` is `unknown` in this version: the highest height whose hash is the same in both histories needs the old blocks, and only the published entries have them (a proposal for the design, correction 213).
* **Not built yet:** the scheduled audit job, and the
  check of the anchors.

### The review of the wiring of anchoring and its fixes (corrections 230 to 245)

The review ([report](../docs/reviews/2026-11-20-kingresd-anchoring-wiring-and-publication-fixes.md)) found one high, five medium, eight low defects and three notes (P7A-01 to P7A-17). Each is fixed with a test that fails for a one-line mutant that brings the defect back; the fixes are not reviewed yet.

* **Restoring the system database (P7A-02, [ADR 0025](../docs/adr/0025-restore-of-the-system-database-is-accepted-by-an-operator.md), design 9.4).** The chain logs are derived again from the chains; the aggregate log and the events are not, and no computation brings them back. A restored database that is behind `published.json` is refused with the critical alert `ledgerSignerUnavailable` (subject `published-state`), which now says what was published, what the database holds and a confirmation of sixteen hexadecimal digits. The procedure: (1) read the alert and check that the database really was restored and that no newer backup of it exists; (2) restart every `kingresd` after the restore of the system database (as before); (3) on the host of the manager leader write the confirmation of the alert, and on a second line a reason of up to 200 printable ASCII characters, into `<ledger-keys-dir>/accept-restore`; (4) the leader makes the record what the database holds, one generation higher, keeps the old record as `published.json.before-restore-<generation>` (written whole into a temporary file, synced and renamed, with the directory synced, and before anything changes; a copy that is already there is never written over, the new one then gets a suffix `.1`, `.2`, ...: P7R-11), removes the file, and publishes the signed event `restore-acknowledged` before it signs anything else. The other holders take the new generation when the leader sends its record. The logs fork against a mirror that holds the old checkpoints; the event is the public statement of that. A confirmation of another situation is ignored. Two properties of this that are accepted and stated (P7R-11): the event is announced at the start of the next round, but a key task may append another event between the acceptance and that round, so the event `restore-acknowledged` is not always the first event after the acceptance (the record is consistent either way, and the verifier reads the event wherever it is); and a cleared note of an accepted restore can come back from a holder that still has it, which is harmless because the event is keyed by `generation:<n>` and publishing it again finds the existing one.
* **The due list (P7A-01).** `Store.Due(set, perTarget)` selects by state and by due time in the query (pending for the submit job, submitted and claimed for the upgrade job) and limits to 100 records for each target; tests on the memory store and on PostgreSQL.
* **Credentials (P7A-03).** The job `ledger.anchor.credentials` (every 60 s, on the leader) fetches the files this server lacks from the other holders (`credentials/fetch`), gives the holders the files they lack (`credentials/list` says which) and takes away the files of deleted targets; what it cannot do is the alert `ledgerAnchorFailed` with the subject `target:<id>|credentials`. A target that cannot be built fails its records at once (`target_unbuildable`) with the cause in the alert; Retry after the fix. **Only when every other holder answered that it has no file** (P7R-08: the answer 404 with the text "This server has no credentials file for the target."; a server that is alone is in this case too): when a holder did not answer, or answered anything else, the file may exist, so the records of the target are retried with backoff (`credentials_unreached`, class retryable, the usual retry limits apply), the alert names the holders that could not be asked, and the job asks again every minute; a new leader whose only file-holding predecessor is down therefore does not fail the records for good. The job takes the files of a target away from the holders only for a target that is absent from a list of the targets read **after** the holders were asked (P7R-10: `CreateTarget` commits the row first and writes the files after, so a file that the report shows belongs to a row that exists when the list is read; a target that is created while the job asks is never taken for an orphan). A late worker of an earlier term that met `ErrDeposed` no longer ends the first pass of a later term of the same server (P7R-03: the error is kept with its term). The REST answer of a create or update does not change (201, 200); a failed distribution is an alert within 10 s. **A holder that holds an older file is brought up to date (P7F-01, P7G-02):** every credentials file carries a rotation generation (a counter, 0 for a file of an earlier version) and the stamp of its rotation (16 random bytes), inside the file (inside the sealed text of a sealed file); a rotation writes a generation one above the highest that the leader and the holders it could ask hold. `credentials/list` also says, for each file, the generation and a digest (an HMAC keyed with the stamp, so that a digest that leaks cannot be tested against guesses of a secret, compared in constant time). No time decides anything: not the time of the file system (a restore from a backup resets it) and not one that a holder reports. The job gives the file to a holder with a lower generation, takes the file of a holder with a higher generation before it gives anything away (`credentials/fetch` of that holder), gives a file of the same generation that differs to nobody (the alert asks the operator to enter the credentials again, which makes a higher generation), and a server refuses to write a file of a lower generation over a higher one, or another of the same generation (`409 older_generation`), whoever sends it. The job keeps the alert while a holder cannot be asked or was not given the file, and clears it only when the pass saw every holder current or gave the file to every holder that was behind. A worker asks the holders before it builds a target (at most once in 30 s for each target and term) and takes a newer file first; when a newer file exists and cannot be taken the record waits (`credentials_stale`, class retryable) and nothing is sent with the old credentials; when the holders cannot be asked at all (the old leader is down) the worker goes on with its own file. A file that a holder gave and this server could not write is `credentials_unreached` (retryable), no longer the final `target_unbuildable`. A newer file that cannot be taken is waited for 30 minutes (`credentials_stale`, retryable) and then fails the records for good with an alert (Retry after the fix); a file that does not open here (another passphrase) fails them at once; a passphrase file that cannot be read is a matter of this server (retryable, like a full disk). The event `restore-acknowledged` now has the subject `generation:<n>:<first 16 hex digits of the sum of the replaced record>` (P7F-07), so a second restore that reaches the same generation number is announced too.
* **Passes (P7A-04).** One worker for each target, the pass waits 20 s at most; a hung target keeps its worker and does not hold the others.
* **Evidence (P7A-05, P7A-09).** A receipt is compared with the record before it makes it anchored (the key and the sum of the note, the digest, the imprint of the token); the evidence file `json` is for an anchored record only (404 otherwise), with the time of the anchoring.
* **A chain that answers another chain id (P7A-06)** is retired only when the same id answers in every round for 30 minutes (the same time as for a database that vanished); an alert from the first answer. The clock is in memory (P7A-16): a change of leader starts it again. This is in `ledgerround/chain.go`.
* **Results of old configurations (P7A-07), stale failures (P7A-08), redaction (P7A-10), the text of a `credentialsPath` failure (P7A-12), concurrent enqueue (P7A-13).** As in corrections 236 to 243 of the design. A TSA username and password are at least four characters.
* **The record of what was published (P7A-11, P7A-14).** A write that fails (disk full, directory gone, more than 32 MiB) leaves the record in memory current and the file behind; nothing is signed, and the critical alert stands, until it is written (it is tried before the next signature). The rename is followed by a sync of the directory.
* **Decisions (P7A-15).** Written in correction 244 of the design: paths of `credentialsPath` and `caBundlePath`, addresses as IPs, what `kingres_readonly` reads, the probe objects of the Test in a bucket with a compliance retention, and `anchor_minimum`. They are decisions that stand, not defects.
* **Not done:** a recovery of the lost rows of the aggregate log and the events from a mirror (it would keep the history unbroken; ADR 0025 names it); a console action for the acceptance; a cluster test of a restore of `kingres_cp` from a backup.
## Table lifecycle (contract 0.14.0, increment 9.6 of the shard design)

`distributeTable` (changed: a table with rows can be distributed), `undistributeTable`, `alterTableDistribution` and the five partition operations
(`listTablePartitions`, `createTablePartition`, `attachTablePartition`, `detachTablePartition`, `dropTablePartition`) start tasks of kind `table.distribute`,
`table.undistribute`, `table.alter` and `table.partition` (subject `table`, id `database/schema.table`); `previewTableChange` answers synchronously. The code is
in `internal/ops/tablelife.go` (steps and the database side), `internal/ops/tableprep.go` (what is checked before a task exists), `internal/httpapi/handlers_tables.go`
and `internal/api/tables_lifecycle.go`. They need `kingres_shard` 0.7 with its increments 9.3 to 9.9; without the report function the operations answer 422 `unsupported_operation`.

- **Before the task exists.** The request is validated, the table is read, and `kingres.table_change_report` runs for the mode the task will use; a blocker is a 422
  `table_not_eligible` with `blockers` (the lines of the report). `mode` `auto` is online when the online report has no blocker and the table has more than
  100000 estimated rows, otherwise blocking; `online` and `blocking` ask for one and are refused with the blockers of that mode. An unfinished rebalance
  answers 409 `rebalance_in_progress` (a rebalance that starts later is not stopped by a table task yet: see "Not done"). A busy colocation group is refused by the extension
  and the task fails retryable with code `operation_in_progress`.
- **Steps (online).** Check the table, (Find linked tables for an undistribute, Plan the change for an alter), Check free space, Prepare (`begin_table_operation`, which finds
  an operation that is unfinished for the table, so the step is idempotent), Create the new layout (`table_operation_advance(..., 'shadow')`), Copy rows, Catch up with writers,
  Switch, Verify and clean up. Blocking: Check the table, Check free space, Copy rows and switch (one call of the blocking function; writers wait), Verify and clean up. Partition tasks:
  Check the request, Change the partitions on every server, Verify and clean up. A task recorded before this version (input without `v`) still runs its one step.
- **Free space.** The estimate (rows and bytes of the table with its partitions; a distributed table's local partitions times the groups) is compared with the free space of each
  destination server (a share of the data plus half again plus 64 MiB; an undistribute needs the whole table on its target). `Ops.DiskFree` tells it; `kingresd` knows the data directory of
  its own server only, so other servers are not checked from here and the step is skipped when this server has no probe.
- **The switch and `paused`.** When the attempts of the extension are used up (a long transaction holds the table) the step shows the blockers in the task's detail ("Waiting to switch: ...")
  and tries again every 60 seconds, for as long as they hold (a day, then the task fails retryable with `switch_not_possible`). It does not use the engine's paused state: nothing resumes a paused table task, so the wait stays inside the step;
  a cancel request is looked at between the attempts.
- **Cancel and failure.** `Cancellable` is true until the switch is done (a blocking task: until its one call runs). A cancel, or a failure, of an online task that has not switched calls
  `cancel_table_operation`, so nothing is left; a node that cannot be reached keeps its leftovers, named by the extension, for the recovery worker. A task killed with the process goes on at the first step that is not done (tests: `internal/ops/tablelife_test.go`, a crash at every step).
- **dependentViews.** `recreate` sets `kingres_shard.dependent_views` for the operation (`SET` on the service connection of the blocking call, and in the options of `begin_table_operation`, which keeps it in the operation); `refuse` is the default.
- **Table.partitioning** (optional) is read from `kingres.dist_table` by `listTables` and `getTable`.

Tests. Unit (no database): `go test ./internal/ops ./internal/httpapi ./internal/contract ./internal/problem ./internal/api` (a fake database side: steps, crash at every step, cancel while the switch waits,
blockers, free space, validation). Integration, with a cluster: `internal/clustertest/tablelife_test.go` (`TestTableLifecycle`: distribute with rows blocking and online with a writer on another server, a table
that is refused, alter, undistribute, a cancel, a switch held by a transaction, partitions, views), run only with `scripts/run-integration.ps1 -Packages ./internal/clustertest -Run TestTableLifecycle` after the extension of this
increment is installed in the installation of the tests.

Not done: the rebalance does not look for an unfinished table task before it moves a bucket of the group (the extension refuses a move of a group that an operation claims; the rebalance then reports the move as failed); the free space of other servers.

### Distribute with related tables and the operation indicator (contract 0.32.0)

`internal/ops/tablerelated.go`: the foreign key closure of a plain table (`Graph`: a recursive query over `pg_constraint` that does not go through a table in `kingres.dist_table`, behind the extension guard), the pure planner (`planRelated`: order, the column each table gets from the keys, `related_column_unknown`; `checkChoice`: `related_table_missing`, `related_table_unrelated`), `listRelatedTables`, and the task. `PrepareDistribute` with `relatedTables` captures the keys (`pg_get_constraintdef`, `convalidated`) in the input of the task, refuses `mode` `online`, a busy table and a report blocker that the removal of the keys does not clear (`filterSetBlockers`), and the task is `table.distribute` with the steps *Check the tables*, *Check free space*, *Take the foreign keys between the tables out*, one *Distribute schema.table* per table (the table first with its own `colocateWith`, the others `colocate_with` the table), *Add the foreign keys again* (`NOT VALID`, without it when the extension refuses that), *Validate the foreign keys*. Every step checks what exists (`kingres.dist_table`, `pg_constraint`), so a task that is run again skips what is done. A failing step puts the keys back (`AddKeys` of the missing ones) and names the keys that it could not put back in the task error. Cancellable until the keys are taken out (`tableCancellable`).

`internal/ops/tableops.go`: `TableOperations(db)` builds `Table.operation` from the active tasks with subject type `table` (kinds `table.distribute`, `table.undistribute`, `table.alter`, `table.partition`, `table.setStorage`, `partition.maintain`, `table.export`, `table.import`; the related tables of a distribute with related tables with role `related`) and from `kingres.table_operation` and `kingres.table_operation_item` (read after `extUsable`; an untrusted or absent extension gives no records; a record of a table that has a task is left out). `listTables`, `getTable` and `listRelatedTables` are leader-bound reads (`forward.go`). Tests: `tablerelated_test.go`, `tableops_test.go` (in memory), `tablerelated_pg_test.go` (a real catalog with a stand-in of the schema `kingres`; skips without `KINGRES_PG_BIN`).

## Elasticity (phase 10, contract 0.15.0): strategies and weights, drain, automatic mode, split

Design: [shard-phase10.md](../docs/design/shard-phase10.md), sections 4, 6, 8, 9, 11 and 21.11. The first increment (the six rebalance operations by bucket count) was built earlier (contract 0.9.0);
this is what 0.15.0 adds. All of it needs the sharding extension 0.8; against an older one the plan stays the plan by bucket count (`kingres.rebalance_plan()`) and anything else is refused with
422 `unsupported_operation`.

**Strategies, threshold, weights (increment 2).** `rebalance.Options` has `strategy` (`byBucketCount`, `byDiskSize`), `threshold`, `drainOnly`, `maxMovesPerServer`, `acknowledgeFailures`,
`cleanupDelaySeconds` and the trigger of the run; `Resolve` fills what a request leaves out from the cluster settings (`kingres_cp.cluster_setting`, keys `rebalance.*`) and the default threshold of the
strategy (0 by bucket count, 0.1 by disk size), so the run records what it really used. `PG.Plan` calls `kingres.plan_rebalance(strategy, threshold, NULL, drain_only, extra_load, exclude_groups, NULL)`
on the map group primary of the database, with the load of the other databases as `extra_load` (their `group_load`, matched to the groups of this database by the host and port of the primary, because group numbers belong to one
catalog each), and `excludeServerIds` as `exclude_groups`. `previewRebalancePlan` adds the strategy, the threshold, `imbalance` (the worst deviation of a receiver, from `group_load`), `loadBefore` and `loadAfter` per
server and the warning `groupUnavailable`. The weight of a server is `kingres.node.weight` (the same on every node row of the server); `PATCH /servers/{id}` with `weight` writes it with
`set_node_property(node, NULL, weight)` and `Server.weight` reads it (absent when the column does not exist). The new settings are `rebalance.strategy`, `threshold`, `maxMovesPerServer`, `maxMoveAttempts`,
`maxDiskUsedFraction` and `maxStandbyLagBytes`; `updateClusterSettings` takes them in `RebalanceSettingsUpdate` (every member optional). Migration `0040_rebalance_phase10.sql` adds `reason` and `cost` to
`rebalance_move` (it has a number range of its own so that it does not collide with the failover and ledger migrations).

**Drain (increment 3).** `previewServerDrain` and `drainServer` (`internal/rebalance/drain.go`). *Departure from the design:* a drain is a task of kind `rebalance` (subject `cluster/cluster`) whose options say
trigger `drain` and name the server, not a task of kind `server.drain`. The exclusion of server operations (the triggers of migrations 0009 to 0016), the stop, the resume, the report, the alert and the pause of a
rebalance by a failover then hold for a drain without a second copy; a drain cannot run beside a rebalance, which the design wanted anyway. The task has the extra steps that only a drain runs (a rebalance
skips them): `Stop new buckets from landing on the server` (the flag goes to false first), the usual `Compute the plan` and `Move buckets` (with the server added to the excluded groups), `Wait until the server owns no bucket`
(no bucket on its groups, and no move away from it still in `cleaning`, which lasts as long as `move_cleanup_delay`), `Write the report` and `Remove the server` (only with `removeWhenDone`: the removal is the task of
`removeServer`, started after the drain task has ended because the two exclude each other; if the manager restarts in between, the server stays drained and the operator removes it). Refused before a task
exists: an unknown server (404), `last_server`, `no_receiving_server`, `server_hosts_map_group`, `replicas_cannot_be_kept` (fewer servers than `replicaCount + 1` would remain; `acceptFewerReplicas` overrides), `group_degraded`,
`node_state_invalid` for a dead server, and 422 when the server holds ordinary tables that leave with it (`acknowledgeLocalData`). The standbys that the server hosts are relocated by the step `Relocate the standbys hosted on the server` (step 5 of the design): the server
is put in the cluster setting `drain.excludedServers`, `standby.Leader` (hook `Excluded`, `standby.WithoutServers`) leaves it out of the ring, the reconcile creates the replacements on the other servers before it removes the old ones, and the
step waits until `kingres.node` has no standby on the server; a group that no other server can take makes the step fail with `replicas_cannot_be_kept`, or stay with fewer standbys with `acceptFewerReplicas`. A server given back to service (`shouldHaveShards` true)
leaves the setting. **The hand-over of the map group (step 4, `internal/rebalance/handover.go`, contract 0.27.0).** A server that runs the primary of the map group (shard group 0, the manager leader) is drained too, when another server
runs a standby of the map group and the build has the planned switchover (`rebalance.Service.HandOverMapGroup`, wired in `cmd/kingresd/drain_maphandover.go` to `failover.Service.Start`, mode `graceful`, group 0); without either it is
still refused with `server_hosts_map_group`. A planned switchover is a task of the failover class and the database refuses it while a rebalance is pending or running (migration 0009, `KX002`), and the drain is a rebalance, so the step does not
run the switchover inside itself. It writes the marker `drain.mapHandover` (cluster setting: task id, server, state `requested`), records the run as stopped by `safetyRule` and **pauses the task** (a paused rebalance does not exclude a failover);
a background goroutine waits for the pause, asks for the switchover, and records `started` or `refused` (with the problem code and text) in the marker. The switchover stops the old leader's system database cleanly and promotes the standby, so the manager
leader is another server and the task rows, the marker and the pause arrive there with the standby. The job `RunDrainWatch` of the new leader (`ResumeDrain`, every few seconds, leader only) resumes the paused drain once the marker says `started` or `refused` and no
failover task is unfinished (the database refuses the resume while one is); a marker that still says `requested` after five minutes (a leader that ended between the pause and the call) is resumed too. The step runs again from its start: a server that no
longer runs the primary of group 0 ends the step (and clears the marker), and the drain goes on with the standbys (step 5), which now include the former primary of group 0 that the switchover turned into a standby. A server that still runs it (the switchover was
rolled back or given up) fails the task with the code of the refusal, or with `server_hosts_map_group`; the server stays `draining` and the drain can be started again. A switchover that stays unfinished (a failover task paused for an operator) keeps the drain paused, and the request is not made while any failover task is unfinished (the hook refuses with `failover_in_progress`, so that `failover.Service.Start` never resumes an unrelated paused failover). The marker has an attempt (a random name) and the states `requested` (the step paused the task), `requesting` (written before the hook is called), `started` (after it returned, tried for a few seconds on new sessions) and `refused`; the goroutine acts only while the marker carries its attempt, a young marker of the same task makes a manual resume pause again without a second request, and a marker that says `requested` or `requesting` is resumed by the watch when it is older than five minutes or when a failover task created after it has ended (the record of `started` cannot always be made: the switchover stops the system database of the old leader). The age of a marker is read by the clock of the database, so the clocks of the servers do not matter. The marker is cleared when the step ends, fails, or the task fails, is cancelled or ends. The
task keeps its id across the leader change. Cancelling the task keeps the flag false (the server stays `draining`).

**Automatic mode (increment 4).** `internal/rebalance/automatic.go`. The job `RunAuto` runs on the manager leader (`IsLeader`) every `checkIntervalSeconds`; it does nothing while `rebalance.automatic.enabled` is false (the default).
The decision is the pure function `Evaluate(settings, facts)`: the window; the plan with the automatic strategy and threshold (default 0.15); the imbalance held for `settleSeconds`; the last change of the servers older than
`settleAfterTopologySeconds`; `minIntervalSeconds` since the last automatic run (waived by a server that waits for a rebalance and by a drain flag); and no safety rule (`SafetyBlockers`: a failover, or a primary changed within `failoverSettleSeconds`,
a group without a primary, without the standbys the replica count asks for, or with a standby that is behind its primary by more than `maxStandbyLagBytes` (default 64 MiB at the start or the resumption of a run; inside a run `autoGuard` needs more than twice the limit on three evaluations in a row, one before each move, because the run's own writes make the standbys lag; the lag is read on the primary of each group, all groups at the same time, each bounded by 15 s: the largest distance between the write position and the replayed position of the physical standbys that are catching up or streaming (`pg_stat_replication` joined with `pg_replication_slots` of type physical; a base backup, a logical walsender and a sender without a slot are ignored; cascading standbys are not senders of this primary and are not seen); a standby that has no replayed position yet, and a primary that cannot be asked, count as lagging; `Service.StandbyLag` is the hook, `LaggingGroups` the pure rule), a server that is dead or suspect or an unfinished change of the servers, a failed earlier move that nobody acknowledged, a cluster check that fails).
The timers are rows of the system database (`autostate.*`, `topology.changedAt`, the end of the last automatic run), so a new leader does not restart them. An automatic run asks `autoGuard` before each move and
pauses (stopped by `safetyRule`) when a rule fails; the job resumes it when the rules have held for `settleSeconds`; a run that an operator stopped is left alone. A manual start is refused with 409 `group_degraded` (the groups are named) while a group has a standby over the same limit or whose lag cannot be read, unless the request says `force: true` (`Service.manualLagGate`; a drain, an automatic run and a cluster that wants no standbys are not asked; contract 0.27.0). A failed automatic run raises the alert `rebalanceFailed`
and suspends the mode until an operator starts a rebalance (a manual start acknowledges it). `RebalanceStatus.automatic` shows the last evaluation.

**Split (design step 6).** `previewBucketSplit` and `splitBucket` (`internal/rebalance/split.go`), task kind `bucket.split` (subject `bucket`, id `database/colocationId/bucket`): `Check that the split can start`,
`Split the bucket` (calls `kingres.split_bucket` in a session of its own and follows `kingres.bucket_split` for the detail; a lost lease or a cancel calls `cancel_bucket_split`; a step repeated after a takeover finds the record and
settles it with `resume_bucket_splits()`), `Check the new buckets`. The orphans are dropped by the extension after `move_cleanup_delay` (`cleanup_bucket_splits`, called by its recovery worker); the task does not wait for that.
**Cancelling a split, and its limit.** The extension looks at a request to cancel (`cancel_bucket_split(id)`) only during the backfill, before each batch of rows is copied; after the backfill it cannot stop the split, and it refuses the request while the split is switching.
The step `Split the bucket` is not interruptible by the engine (an interrupted step ends the task as cancelled whatever the extension does). The step itself watches for the request (every second), passes it on and says truthfully what came of it:
(a) the request reached the backfill: the extension undoes the split (the children and the triggers go, the bucket is `active` again) and the task **fails with the code `cancelled`** and the text "The split was cancelled before its switch and undone: the bucket is unchanged"; the bucket can be split again;
(b) the request came too late (the extension refused it, or accepted it and the split completed all the same): the task records in its detail "The cancel request came too late: the split had passed its backfill and goes on to its end", **goes on and ends `done`** (the step `Check the new buckets` runs), and the bucket list shows the children. A task is therefore never `cancelled` while the split completes.
The cancel guard of the kind (`splitCancellable`) accepts a cancel until the split step is done or the "too late" note is recorded; a cancel after that is answered 409 `task_not_cancellable`, which is the truthful answer. A split whose session is lost (a restart of `kingresd`) is not touched by a lost lease or a shutdown: the task is taken over, finds the record and settles it with
`resume_bucket_splits()`. The cluster test of the split shows both cases (the gate `split_after_triggers` for the undo, `split_after_backfill` for the late cancel, which ends done with the children in the bucket list).
The preview does not look for a table without a primary key: the extension refuses it when the split starts and the task fails with `split_unsupported`. `listColocationGroups` and `listBuckets` read `max_modulus`, `modulus` and `kind`.

Tests: unit tests in `internal/rebalance` (`options_test.go`, `drain_test.go`, `automatic_test.go`, `split_test.go`, `handover_test.go`, `lag_test.go`; with PostgreSQL: `handover_pg_test.go`, `lag_pg_test.go`), `internal/ops` and `internal/api`; the cluster tests of the hand-over of the map group and of the lag rule `internal/clustertest/phase10_drain4_test.go` (a drain of the manager leader with writers, a switchover that is given up, an automatic run held back by a standby that lags); the cluster test `internal/clustertest/rebalance_strategies_test.go`
(strategies, weights, exclusion, settings, a drain-only run) needs the 0.8 extension in the installation of the tests (`scripts/install-extensions.sh`).

## Distributed schemas (phase 11, step 13, contract 0.15.0)

Design: [shard-phase11.md](../docs/design/shard-phase11.md), sections 13 and 16. The extension (0.8) does the work; `internal/ops/schemas.go` lists the schemas, refuses what can be refused before a task exists and runs three tasks,
`internal/httpapi/handlers_schemas.go` has the five operations (`listSchemas`, `getSchema`, `distributeSchema`, `undistributeSchema`, `moveSchema`).

- **Reading.** A schema is listed from the map group primary (this server): every schema that is not a system schema, joined with `kingres.schemas` (group, state, size, target of a move) when the extension has it; a database without the extension 0.8, or
  without sharding, lists its schemas with `distribution: null`. `getSchema` adds the blockers (`kingres.schema_inventory`, the unsupported rows), the pin (`kingres.internal_schema_pinned`, for example a ledger table), the unfinished `schema.move`
  task of the schema and the tables that have neither a primary key nor a replica identity.
- **Tasks** (subject type `schema`, id `database/schema`, one unfinished task per schema): `schema.distribute` (`Check the schema`, `Set REPLICA IDENTITY FULL` when asked, `Make the schema distributed` through `distribute_schema`, `Verify on every server`),
  `schema.move` (`Check the move`, `Copy the data` through `move_schema` in a session of its own with the phase of `bucket_move` as detail, `Switch`, `Clean up the old copy`; cancelled with `cancel_bucket_move` until the switch, the task does not wait for the
  old copy to be emptied) and `schema.undistribute` (`Check the schema`, `Move first` when a target is given, `Make the schema local`, `Verify on every server`). A step that repeats after a restart reads the catalog first and does nothing when its result is there.
  `Make the schema distributed` and `Make the schema local` are `NonIdempotent` with a `Recover`: a leader killed inside the call of the extension (which prepares a transaction on the other nodes first) leaves the step recorded `running`; the next leader reads whether the schema is distributed (or local), records the step `done` when it is, and otherwise asks `kingres.recover_prepared_transactions()` to settle what the cut call left prepared before the call is made again.
- **Refusals before a task exists:** `schema_not_distributable` (422, the objects in `errors`), `conflict` (already distributed, local, or the mode is `row`), `schema_move_in_progress`, `schema_pinned`, `rebalance_in_progress` (a move waits for a rebalance),
  422 `unprocessable` (target group missing, not receiving, or the current one; tables without a key unless `setReplicaIdentityFull`).
- **Tables that a move cannot copy** are those whose replica identity is NOTHING (a primary key does not help) and those with the default identity and no primary key; the 422 names every one of them in `errors` (code `replica_identity_missing`) and in its text.
  The same check, without the way out, refuses a `Make local` that names a group to move to first (`undistributeSchema` has no `setReplicaIdentityFull`): no task is made that would fail in its second step.
- **Exclusion.** Migration `0041_schema_tasks_exclusion.sql` is a second trigger on `kingres_cp.task`: the three schema kinds and a failover (or the restore of preferred primaries) exclude each other (KX007 for the schema task, KX006 for the failover), and `schema.move` and
  `schema.undistribute` (which may move the schema first) and a rebalance exclude each other (KX002, KX001); `schema.distribute` moves nothing and lives beside a rebalance. They also exclude `server.add`, `server.remove` and `server.leave`, both ways (KX007 for the schema task, KX006 for the server task), since a move to a group whose server is being joined or removed must not run. The answer is 409 `operation_in_progress` naming the other kind (a rebalance that is refused keeps
  `rebalance_in_progress`); `task.MemStore` follows the same rules. A paused failover or rebalance still excludes, as for the other kinds.
- **Cancel.** The step `Copy the data` (and `Move first`) is not interruptible: it polls the cancel mark itself (`task.Env.CancelRequested`) and asks the extension to stop the move only while the phase is `preparing`, `copying` or `catching_up` (`cancel_bucket_move`).
  The answer is truthful: the extension undid the move and the task **fails with the code `cancelled`** and says the schema is unchanged, on its old group; or the request came in the switch (the extension refuses), the task goes on, ends `done` and says so in its detail.
  The guard (`moveCancellable`, `undistributeCancellable`) refuses a request (409 `task_not_cancellable`) from the phase `switching` on, read from the detail the step records, and once the copy step is done; the mark of a request that arrived before is not followed.
  A cancel of a `Make local` that is moving first is a cancel of that move; once `Make the schema local` has started, there is nothing to cancel. `getTask`, `listTasks` and the answer of the cancel narrow `cancellable` with the guard of the kind (`withCancelGuard`), so the console shows no Cancel button for a move that is switching.
- **After a leader change, or a dead session** the copy step finds the move that nobody drives in `kingres.bucket_move` (a record that a dead leader left, or one whose call returned an error): the task asks the extension to settle it, now and then again every twice `kingres_shard.node_timeout` plus a second (`resume_bucket_moves()` skips a move in preparing or switching younger than that), and gives up ten minutes after the call ended; one to the same group is followed, one to another group is a `schema_move_in_progress`,
  and a move that ended `failed` is started again, up to three times, unless a person asked to cancel.
  A failed call of `distribute_schema` or `undistribute_schema` whose schema is in the state the call was to make counts as made (a backend of a dead leader may have finished it while the step was recovered). `Make the schema local` after a `Move first` ends the cleaning of its own move at once (`internal_continue_cleaning`), because the extension refuses while a move of the schema is cleaning; `Move first` records where the schema went, which is what a cancel before the making local leaves. Only SQLSTATE 55000 of `cancel_bucket_move` means too late; other failures are tried again.
- **The sharding mode.** `createDatabase` takes `sharding.mode` and `distributeNewSchemas` and calls `kingres.set_sharding_mode` after the extension is installed; `updateDatabase` changes them synchronously, and a refusal of the extension is 409 `sharding_mode_in_use`.
  `Database.sharding` shows `mode` and `distributeNewSchemas` (read from `kingres.sharding_settings`), `bucketCount` is null in mode `schema`, `stats.distributedSchemaCount` counts `kingres.dist_schema`, `getDatabaseCreateOptions` lists `shardingModes`.
  `listColocationGroups` has `kind` and `schemaName`.
- **Since contract 0.31.0:** a rebalance and a drain plan and move distributed schemas as items of the plan (`kind` and `schemaName` in the plan, `inFlightMoves[]`, the drain preview and the report). A drain refuses before a task exists for a schema that holds a ledger table (409 `schema_pinned`) or that a move cannot copy (422 `unprocessable`), and it ends only when no bucket and no schema is left on the server.
- **Not done:** `BucketDistribution.servers[].primarySchemas` (the contract has no such member).

Tests: `internal/ops/schemas_test.go` (blockers, error mapping, hostile names, the cancel guard) and the cluster test `internal/clustertest/schemas_test.go` (list, blockers, distribute, move with the rows checked, make local, the replica identity,
the mode refusal), `internal/clustertest/schemas_tasks_test.go` (tables a move cannot copy, the cancel before and after the switch, the exclusion of a rebalance and a failover, a move followed after the leader was killed, the distribute step killed before and after its effect), `internal/task/recover_test.go` (`Recover`, `CancelRequested`, the exclusion in the memory store) and `internal/rebalance/failover_exclusion_pg_test.go` (the trigger of migration 0041); they need the 0.8 extension in the installation of the tests.

## Analytics: approximate aggregates, storage and time partitioning (phase 12, contract 0.17.0)

Designs: `kingres/docs/design/analytics-phase12-queries.md` section 12 and `analytics-phase12-storage.md` sections 10.6 and 13. The contract is revision 0.17.0
(11 operations, three task kinds, six problem codes, two alert kinds); it is revision 0.17.0 (written as 0.18.0 on the branch and renumbered by the integrator of g16; the migrations stay 0035 and 0036). Every operation acts on the tables and databases of **this server's instance**, where the
manager leader runs: a plain table that lives on another group's instance is not found here (distributed tables and their partitions are a later step,
D1a and D1b of the design).

### Preload (the fix that the analytics needed)

`shared_preload_libraries` of every instance is `kingres_shard,kingres_ledger,kingres_columnar,pg_stat_statements` (`instance.PreloadLibraries`). Without `kingres_columnar`
nothing can write a columnar table and `kingres_timepart` refuses every conversion; `kingres_timepart` and `kingres_sketch` need no preload. An instance made
by an earlier package has the old list in `kingres-managed.conf`: `EnsureConfig` (run when `kingresd` starts) rewrites the file when the list differs, and
the new list takes effect at the next restart of the instance. A server whose instance has not been restarted answers `409 extension_missing` (naming the
library and the server) to a storage conversion, and says what to do.

### pg_stat_statements and plpgsql in the first release

`shared_preload_libraries` of every instance is now `kingres_shard,kingres_ledger,kingres_columnar,pg_stat_statements` (`instance.PreloadLibraries`). The
library is the second one that the installation may lack (`optionalLibraries` in `internal/instance/configfile.go`): the list of an instance leaves out a
library whose file is not in `pg_config --pkglibdir`, so that a package that was not upgraded with `kingresd` still starts. The two differ in one rule:

- `kingres_columnar` is **sticky**. An instance that has had it keeps it, even when the file is missing now (the marker file `columnar-preload`), because
  columnar tables cannot be read without the resource manager.
- `pg_stat_statements` is **not sticky**, and has no marker. It holds no data that anything needs, so a missing file must never be what keeps an instance from
  starting: the next list that is written leaves it out, and the `EnsureConfig` after the file is back puts it in again. (The marker logic of columnar is
  followed everywhere else: the library-exists check, the warning, the reload.)

**Upgrade of an existing instance.** `EnsureConfig` (run when `kingresd` starts and by the configuration agent) sees the old list in `kingres-managed.conf`,
logs a warning, rewrites the managed file from the stored settings (every other setting is kept), and reloads a running instance, so that PostgreSQL shows
`shared_preload_libraries` as pending a restart (`pg_settings.pending_restart`). The list takes effect at the next restart; the restart of the pending
instances of the console puts standbys before their primaries. Until then `pg_stat_statements` collects nothing and its views fail with "must be loaded via
shared_preload_libraries"; nothing else is affected. A file that already has the right list is not rewritten.

**Which databases have the extension.** The ones that `kingresd` makes: `postgres` and `kingres_system` when the cluster is created (`installStatStatements`,
a package without the extension leaves them as they are), and every database made by `database.create` (`installAnalytics`, on this server and through the
apply kind on the others). A server that is added gets the extension with each database of the leader (`globals/apply` copies the extension list).
`ExtStatStatements` is in the closed list of `extension.check`, `extension.install` and `extension.verify`.

**Copying extensions to an added server (`globals/apply`).** The leader sends, for each extension of a database, the schema it is installed in and the
owner of that schema (`ExtensionPlaces`). The receiver checks the package first (`pg_available_extensions`): `pg_stat_statements` that the package lacks is a
warning, any other extension that it lacks is one error that names all of them; every database and every extension is tried, and the errors come back
together. For each extension it runs `CREATE SCHEMA IF NOT EXISTS ... AUTHORIZATION owner` and `CREATE EXTENSION IF NOT EXISTS ... SCHEMA schema` in one
transaction, quoting both names; a schema that exists on the receiver with another owner than on the leader is refused. An extension whose control file
fixes its place (it names a schema, or `relocatable = false`: the `kingres_*` extensions) and an extension in `pg_catalog` get no clause. **Mixed
versions: upgrade the leader last.** A leader of an earlier version sends no places; the receiver then makes `pg_stat_statements` in `public` and any
other extension without a clause, as that version did, so an extension outside `public` or `pg_catalog` that is not fixed is only copied correctly
from a leader that has this change.

**Databases that exist already** get no extension from `kingresd`, neither lazily nor at start. That is a decision: the storage extensions are installed at
first use because an operation of `kingresd` needs them, and nothing in `kingresd` reads `pg_stat_statements`, so there is no first use to hang the
installation on, and `CREATE EXTENSION` run by the control plane in a database of a user at an unforeseen time (a superuser statement on the objects of the
user, a catalog change that every standby replays) is a surprise that is not worth a view. The administrator runs `CREATE EXTENSION IF NOT EXISTS
pg_stat_statements` once in each database that should have it, on the primary of every server group (a standby gets it through replication; a database of a
server that is added later gets it with the globals). The guide says so.

**Configuration.** The settings of the extension are not managed and not refused: `pg_stat_statements.max` (restart), `pg_stat_statements.save` (reload),
`pg_stat_statements.track`, `.track_utility` and `.track_planning` (also per database) can be set at cluster, server group and server level, and none must
be equal on every server. `shared_preload_libraries` itself stays managed (its reason names the library). The statement texts that the extension keeps live in
shared memory and in `pg_stat/pgss_query_texts.stat`, never in a log; the rules that keep statement text out of the logs are unchanged.

**plpgsql** is part of the server, is in `template1` of every cluster, and so in every database that `kingresd` makes (the databases are made from
`template0` with the extensions installed afterwards; `plpgsql` is created by initdb in `template1` and `postgres`, and `database.create` takes
`template0`, which has it too: the cluster test checks every database on every server). `kingres_shard` does **not** propagate `CREATE FUNCTION`: a function is a
local object on each server (the list "Propagated" in the README of `kingres_shard`, "DDL on distributed and reference tables", names only statements on tables, and a function of a user is never shipped inside a query). A plpgsql function is
used against a distributed table from every server, in a filter, in a read and in a write; the same function has to be created on every server, and
`TestStatStatementsAndPlpgsql` pins both facts.

### Extensions in databases and on new servers

`database.create` installs `kingres_columnar` and `kingres_timepart` (`installAnalytics`, on this server and, through the apply kind `database.create`, on every
other server) when the package of the server contains them; an older package makes a database without them, as before. `kingres_sketch` is not installed by
`database.create`: the task `database.approximateAggregates` installs it. A server that is added gets every extension that is installed in a database of the
leader (`globals/apply` already copies the list), the three of this phase included, and the database settings (`ALTER DATABASE ... SET`) with them. A database
that was made before this version gets `kingres_columnar` and `kingres_timepart` at the first use of a storage or policy operation (`ensureExtensions`; a
deviation from the design, which answers `extension_missing` for an extension that is not installed: there would be no way to install it from the console).

New closed kinds of the internal call `apply` (leader only): `extension.check`, `extension.install`, `extension.verify` and `database.settings`. They carry a database
and an extension name of a closed list (`kingres_sketch`, `kingres_columnar`, `kingres_timepart`) and never SQL. A refusal that matters comes back as an internal
error with the problem code (`extension_unavailable`, `extension_version_mismatch`, `sketch_hash_mismatch`, `extension_missing`), which `healable` does not retry. The
leader reads the version that another server has installed with the internal call `exec` (read-only statement; `ops.RemoteQuerier`, implemented by
`membership.Manager.Query`), so `getApproximateAggregates` is one of the leader-bound reads (a follower forwards it).

### `database.approximateAggregates`

Subject: the database (the subject of `database.update`, so a second `database.*` task on it is `409 operation_in_progress`). Steps: `Check the servers` (this server
first: its default version is the one the others must have; then every other server), one `Install the extension on server <id>` for each server, `Verify versions
and hashes` (versions and `kingres_sketch.self_test()` of every server; it names **every** server that differs, with the code of the first), `Apply the database settings`
(`kingres_shard.count_distinct_mode`, `count_distinct_error_rate`, `enable_sketch_pushdown`, on every server; a server whose `kingres_shard` does not know a setting
skips it and the step says so in its detail) and `Record the settings` (migration 0035: three columns of `kingres_cp.database_setting`). A server that cannot be reached is
waited for, with the server named in the detail, as in every cluster-wide task. Turning it off sets `count_distinct_mode` to `exact` and the pushdown off, resets the error
rate and never drops the extension. `getApproximateAggregates` shows the setting and the installed version of each server (`ok`, `notInstalled`, `versionMismatch`,
`unreachable`); `Database.approximateAggregates` carries the setting.

### `table.setStorage`

`setTableStorage` and `setPartitionStorage` start the task (subject `database/schema.table`, or `database/schema.partition`). Steps for a table: `Check the table`, `Convert the storage`
(`ALTER TABLE ... SET ACCESS METHOD`, one transaction under an `ACCESS EXCLUSIVE` lock, which the step says; the options of the request are set as the `kingres_columnar.*` settings
of that transaction) and `Analyze the table`; for a partition `Check the partition`, `Convert the partition` (`CALL kingres.convert_partition`: the new partition is built aside and swapped in)
and `Analyze the partition`. The request is checked as the first step would check it, before a task exists (`409 conversion_refused` with the reasons: a partitioned table has no storage of
its own, a ledger table, a distributed table, a publication, foreign keys, and for a partition the reasons of `kingres.timepart__convert_refusal`), and again by the step, because the state can
change in between. A table that already has the format is not converted (the step says so). The convert step can be cancelled while it runs; the conversion is one transaction and rolls back.
`listTables` and `getTable` carry `storage` (access method, partition formats, the totals and options of the columnar format) and, for a table that is range-partitioned on one time column,
`partitioning.columnType`, `hasPolicy` and `defaultPartition`; `listTables?storage=` filters (a partitioned table matches when any partition does). `listTablePartitions` also lists the partitions
of an ordinary range- or list-partitioned table, with access method, bounds, size and compression ratio.

### Policies and `partition.maintain`

`getPartitionPolicy`, `putPartitionPolicy`, `deletePartitionPolicy` and `planPartitionPolicy` are synchronous and call `kingres.timepart_set_policy`, `timepart_unset_policy`, `timepart_plan` (and
`timepart__plan_rows` for a policy that is only in the request). A duration is an ISO 8601 text checked by a pattern here and by the extension (the grid, the time zone, the retention against the
partition size), and a refusal of the extension becomes a `400` with the field it names or `409 partition_policy_conflict` (not range-partitioned on a time column, a default partition). A policy that
is put is due at once (`nextRunAt` is now). `createdBy` is the PostgreSQL role that wrote the policy, which is the service role; the audit log has the signed-in role.

`runPartitionPolicy` and the scheduler start `partition.maintain` (subject `database/schema.table`). Steps: `Begin the run`, `Create partitions`, `Convert partitions`, `Retire partitions`, `Finish the run`. `begin`
looks for the run of this task (`kingres_cp.partition_run_task`, migration 0036) before it makes one with `kingres.timepart_begin_run`, and waits while another run of the table is going (the reason is in the
detail); each phase is `CALL kingres.timepart_run_phase`, which records every action before and after it, so a task taken over after a crash goes on with the action it was in; `finish` closes the run. An action
that fails (a lock timeout) does not fail the step: the run ends `partial` and the next run tries again; an SQL error of a step fails the task and marks the run `failed`. The runs are the records of the extension
(`kingres.timepart_run`, `timepart_run_action`) with the task and the role that asked from `partition_run_task`; a run made from SQL is shown as `manual`.

`internal/partsched` is the scheduler: two jobs of `internal/scheduler` (they run on the manager leader only, with its term): `partition.schedule` every `partition-scheduler-interval` (default 1m; environment
`KINGRES_PARTITION_SCHEDULER_INTERVAL`) asks `kingres.timepart_due(now)` in every database that has `kingres_timepart` and starts one task for each table that has no unfinished task (it holds no state: after a change of
the leader the new leader asks again), and `partition.alerts` every five minutes reads `kingres.timepart_check()` and raises or clears `partitionsAheadLow` and `partitionPolicyFailed` (the last run failed, or was
`partial` twice in a row), subject `database:<db>|tableschema:<s>|tablename:<t>` (see below). The counters `kingres_partition_runs_total`, `kingres_partition_runs_failed_total` and `kingres_partition_actions_failed_total` are on `/metrics`.

**A distributed table (St-D1a, St-D1b).** A user-partitioned distributed table is a table of the instance of the manager leader, which holds the map group primary, so its policy, its plan, its runs and the scheduler
are the same operations on the same records; nothing in `partsched` or in the task changed. What differs is inside `kingres_timepart` 0.3: a run creates, drops or detaches the partitions with the partition functions of
`kingres_shard` on every node (a group that a bucket move or a conversion claims is `retry_later`: the action fails with that detail, the run is `partial` and comes back in five minutes), converts the leaves of
an old partition on the nodes that hold them (one action per partition; a leaf that refuses leaves the partition `mixed`, and the next run goes on) and refuses a policy that would give the table more leaves than
`kingres_shard.max_partition_leaves` (409 `partition_policy_conflict`, with the numbers). `partitionColumn` and the health come from the extension, so they name the key of the partitions and not the distribution column.
`listTablePartitions` of a distributed table gives `rangeFrom`, `rangeTo` and an `accessMethod` of `heap`, `columnar` or `mixed`, read on the nodes. A run needs `kingres_timepart` 0.3 in the database of every server that holds a
leaf and `kingres_columnar` for the conversion (`database.create` installs them on every server and the job `extension.update` brings `kingres_timepart` up to date); a server that lacks one makes the conversion `skipped` with the server named. The service role is a superuser, as
the partition functions need. Test: `internal/clustertest/distpart_test.go` (`TestPartitionPolicyOfADistributedTable`).

**The update of `kingres_timepart`.** The job `extension.update` of the leader's scheduler (every ten intervals of `partition-scheduler-interval`, 10 minutes by default, and soon after the leader starts) brings
`kingres_timepart` to the version of the package in every database of this server that has it, and in the same databases on the other servers, with the update path of the extension steps
(`extension.install` of the internal `apply`: `ALTER EXTENSION UPDATE` to the default version of the package of that server, nothing when it is there already), so that a database that was made with
0.2 has the procedure that converts a leaf on every server without a step of an operator. A server that cannot be reached is named in the log and asked again at the next pass. Test: `TestTimepartUpdatedOnEveryServer`.

### Tests

Unit: `internal/ops` (`approx_test.go`: the five steps with fake servers, a version that differs, a package that lacks the extension, the hash, a server that is down, a failure at every call, the process that dies at every
call), `internal/partsched` (a fake clock and backend). PostgreSQL-backed (`scripts\run-integration.ps1 -Packages ./internal/ops,./internal/store/pgstore,./internal/instance -Run ...`):
`TestIntegrationSetStorage*`, `TestIntegrationPartitionPolicyResource`, `TestIntegrationMaintain*` (a run, a locked partition, a task interrupted at each of its five steps), `TestIntegrationPartitionScheduler*`,
`TestIntegrationTableStorage` (pgstore), `TestEnsureConfigBringsThePreloadUpToDate`. Cluster tests of three servers: `TestApproximateAggregates` and `TestTableStorageAndPartitionPolicies` in `internal/clustertest`.
Not covered: a server whose package has another version of `kingres_sketch` (all servers of the tests share one installation; the case is in the unit tests), and the library not preloaded on a running instance
(unit test with a fake backend).

### After the independent review of the analytics control plane (P12C-01 to P12C-16)

The review is in `kingres/docs/reviews/2026-11-22-control-plane-phase12.md`; its "Fixes" section lists every finding. What changed in behaviour:

- **The time of a run is the time of the server (P12C-01).** `runPartitionPolicy` takes `asOf` only within five minutes of the time of the instance (`maintainSkew`), otherwise 400 on `/asOf`; the scheduler goes
  through the same check, so a leader whose clock jumped starts nothing. `planPartitionPolicy` keeps a free `asOf` (it changes nothing). `next_run_at` can therefore never be moved far from now.
- **A run is always closed (P12C-02).** `kingres.timepart_finish_run` runs for every end of a run: the finish step, a run that the extension recorded as failed at `begin` (the owner changed, the policy was never verified:
  the task fails with `partition_policy_conflict` and the reason), an error of a phase (`failRun` marks the run failed and closes it) and, for a failure that no step saw (a cancel, a lost lease), the hook `OnFail` of the kind.
  Closing moves `next_run_at` and prunes the old runs, so a policy that fails is not started at every tick. As a second line the scheduler leaves a table alone for five minutes (`maintainBackoff`) after a failed run.
- **Preload (P12C-03).** `kingres_columnar` is in the list only when the installation has the library file (`pg_config --pkglibdir`): a package that was not upgraded with `kingresd`, or an image that mixes versions, gets the old
  list and its instances still start. `EnsureConfig` logs a warning when it rewrites the list, reloads a running instance (so that `shared_preload_libraries` shows as pending a restart in the configuration view and the
  restart of the console lists it) and repairs a file that names a library that is not there. The list takes effect at a restart: restart the instances with the console's restart of pending instances (it orders the pairs of standbys and primaries by the rule of the configuration design; for this setting the safe order is the standbys of a group before
  its primary, and the restart by hand follows it); a primary that was restarted first and then wrote columnar WAL would be ahead of a standby that cannot replay it. A conversion to columnar is therefore refused (`extension_missing`) until **every server of the cluster** has loaded the library (the leader asks each server with `extension.check`, with the library check), and says to restart the instance of the server it names.
- **One rule for the owners of tables (P12C-04, P12C-05).** The conversion of a table or a partition and the runs of a policy both open the service connection with `kingres_timepart.trust_table_owners = on` (`trustedConn`). The owner of the table is a role of
  this cluster; the code it can cause to run while a partition is made, attached or converted (an index expression, a default, a trigger) runs as the service role for the time of the call, and the connection is closed
  at its end. The owner needs `CREATE` on the schema of the partition. Since the second review this is open only for tables of superusers unless the cluster setting `analytics.allowNonSuperuserOwners` is on (P12D-04); the contract text says so. A refusal by PostgreSQL privileges (42501) is `conversion_refused`, not a retryable step failure.
- **Conversion (P12C-06, P12C-07, P12C-15).** A table that is columnar already gets new options through `columnar_set_options` (for the stripes to come; the step says so) instead of a silent no-op. The `ALTER TABLE ... SET ACCESS METHOD` and the `ANALYZE` ask for their lock
  with `lock_timeout` (200 ms since P12D-05, 3 s at first) and try again, saying "Waiting for a lock held by another session" in the detail, for ten minutes at most, then fail with the retryable `operation_in_progress`; a reader of the table is never queued behind a conversion
  that waits for a long transaction. The claim of another run of the table (55P03 of the extension) is `operation_in_progress` too, and a conversion of a partition is refused while a `partition.maintain` task of its table is unfinished.
- **A run that this instance does not have is an error (P12C-08).** After a takeover by a server whose instance does not hold the run that `partition_run_task` names, the step fails with a clear message instead of ending the task as done.
- **Alerts do not flap (P12C-09).** `Checks` returns the databases it could read; an alert of a database that was not read is left as it is. The alerts carry the table in `AlertSubject.tableSchema` and `tableName` (stored as `database:<db>|tableschema:<s>|tablename:<t>`, with `|` and `%` escaped).
- **The scheduled run no longer needs the library (P12C-10).** Only a run by hand is refused when the policy converts and the library is not loaded; a scheduled run goes on creating and retiring partitions and the extension records what it cannot convert.
- **Reads do not write (P12C-11).** `getPartitionPolicy`, `listPartitionPolicyRuns`, `getPartitionPolicyRun`, `planPartitionPolicy` and `deletePartitionPolicy` only ask whether `kingres_timepart` is installed (404 / an empty list / `extension_missing` for a plan with a policy in the request);
  the writes install it. The reads of policies, runs and `listTablePartitions` are leader-bound.
- **Messages that say the truth (P12C-12, P12C-16).** A table that the leader's instance does not have is 404 with a detail that names the limit; the contract text says it for every operation. `conversion_refused` carries typed `blockers`; `dryRun=true` on both storage operations answers the same reasons as `TableStoragePreview` and installs nothing. The
  remedy for a library that is not loaded is "restart the instance", not "edit shared_preload_libraries" (the setting is managed).
- **Others.** `putPartitionPolicy` is refused while a run goes (P12C-13). `maintainRunning` asks the task store by kind and subject (`Engine.ActiveFor`), not in the newest 200 tasks (P12C-14). Versions of `kingres_sketch` compare as numbers (`0.10` is above `0.9`).
  The steps of `database.approximateAggregates` can be interrupted by a cancel; the list of servers is fixed when the task is made (a server added meanwhile is covered by the next request), and a failure at `Apply the database settings` leaves the settings on the servers that took them: the same request, repeated, finishes it.

### After the second independent review of the analytics control plane (P12D-01 to P12D-09)

The review is in `kingres/docs/reviews/2026-11-22-control-plane-phase12-fixes.md`, with a "Fixes" section. What changed in behaviour:

- **A clock that stepped (P12D-01).** The skew check of P12C-01 compares the leader's clock with the instance's, and both follow the host: a step of the host clock (NTP, a resumed virtual machine, a date set by hand) passed it. A scheduled run is now also
  compared with the history of the policy: a time more than seven days plus the policy's `create_ahead` after the last run (after `updated_at` for a policy that never ran) is refused with a `ClockError`, the policy is not started,
  and the alert `partitionPolicyFailed` says that the clock of the host may have jumped. The seven days are what a stop of kingresd may be; a run by hand, bounded by the skew only, goes on when the time is right.
- **A failure before any run exists (P12D-02).** When `kingres.timepart_begin_run` itself raises there is no run to close. The hook `OnFail` then moves `next_run_at` of the policy by the back-off (five minutes), which survives a change
  of the leader, and the process remembers the failure by table (`failed`), so that four ticks make one task and not four. A begin step that finds the run of an earlier pass already failed closes it and fails with the reason.
- **Standbys (P12D-03).** The library check of a conversion asks every instance of every server (`Local.LocalInstances`), standbys included; a standby that was not restarted blocks a columnar conversion and the message names the instance and says to restart it, standbys first.
- **Owners that are not superusers (P12D-04).** The cluster setting `analytics.allowNonSuperuserOwners` (default `false`; `PATCH` of the cluster settings, `analytics.allowNonSuperuserOwners`) decides whether the conversion of a table and the runs of a policy are open for tables
  whose owner is not a superuser. Off, the conversion is refused with `conversion_refused` (blocker `owner_not_trusted`), a policy cannot be set (`partition_policy_conflict`), a run by hand is refused the same way, and the scheduler does
  not retry such a table: it logs the refusal once and raises one `partitionPolicyFailed` alert for it, which clears when the owner changes or the setting is turned on. `PartitionPolicy` and `TableStoragePreview` carry `tableOwner` and `ownerTrusted`. The trust text of the contract
  is corrected: with the setting on, the administrator trusts the owners of those tables as himself, because the code of the owner (an index expression, a default, a trigger) runs in the session of the service role for the time of a call.
- **The reader stall (P12D-05).** The conversion asks for the lock with `lock_timeout` of 200 ms (it was 3 s) and pauses 3 s between tries, so a reader of the table waits behind the request at most 200 ms at a time. It still retries for ten minutes at most.
- **Alerts of what is gone (P12D-06).** `Checks` reports which databases it could not read; an alert of a database that is no longer there (dropped, extension removed) is cleared, an alert of a database that could not be read is left as it is.
- **The preload list (P12D-07).** The library `kingres_columnar` is left out of the list only for an instance that has no columnar table: a marker file (`columnar-preload`) records that the list once had it, and an instance with the marker keeps the library even when the
  installation lacks it (a warning in the log says so; the instance cannot start until the package that has the file is back, which is better than dropping the library from under columnar tables). `EnsureConfig` writes the marker and reloads a running instance only when the list changed.
- **Leader-ahead skew (P12D-08).** A skew refusal or a refusal for the owner is an issue of the policy and one alert for it, not a failure counted at every tick; the scheduler logs a change of the reason once.
- **Smaller (P12D-09).** `maintainRunning` fails closed (`service_unavailable` when the task store cannot say). The back-off looks at the last run only. Tests cover the registration of `OnFail`, the mapping of 42501 and the reload of a running instance.
