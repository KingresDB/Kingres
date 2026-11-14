<!-- SPDX-License-Identifier: PostgreSQL -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# KingresDB packaging

"One package: install it and the node has everything"
([architecture](../docs/architecture.md), section "One package, and a panel on
every node"). This directory builds the product from the repository and turns
it into a container image, a three-node development cluster and a Debian
package, and tests each of them.

| Artifact | Where | What it is |
|---|---|---|
| Release build recipe | `build/` | One script (`build.sh`) from a clean source snapshot to a staged installation tree under `/opt/kingres` |
| Container image | `docker/Dockerfile` | Minimal Ubuntu 24.04, non-root, `kingresd` as the entry point, health check, starts uninitialized |
| Development cluster | `docker/compose.yaml`, `docker/tokens.sh` | Three nodes, one network, fixed names, published panel ports, volumes |
| Debian package | `deb/` | `kingres_<version>-<revision>_amd64.deb` with the `kingres.service` unit |
| Artifact tests | `test/` | Image, compose and package tests (the package test runs systemd in a container) |
| CI | `../../.github/workflows/ci.yml` | Server, Go, web, contract, shellcheck, and (default branch only) the image and the package |

Target: Ubuntu 24.04, amd64. Everything below was run on Windows 11 with WSL2
(Ubuntu 24.04, Docker Engine installed inside the distribution); see
[What was run](#what-was-run).

## What the package contains

- The PostgreSQL 18.7 based server (`postgres`, `initdb`, `pg_ctl`, ...), its client
  programs, all contrib modules, and the two KingresDB extensions
  `kingres_shard` and `kingres_ledger`.
- `kingresd`: one static Go binary with the web panel embedded.
- `kingres-verify`: the standalone ledger verifier, when `kingres/verifier/go.mod`
  exists in the source tree (the build has a marked hook and skips it
  otherwise).
- The systemd unit `kingres.service` and default configuration (package only).

Not included yet: see [What is not included](#what-is-not-included-yet).

## Build

### Requirements

An unprivileged Linux user (Ubuntu 24.04), the packages of
`kingres/dev/setup-ubuntu.sh` plus `libsystemd-dev libxml2-dev liburing-dev
libnuma-dev uuid-dev dpkg-dev file`, and the pinned Go and Node.js of
`build/versions.env`:

```
build/toolchain.sh ~/toolchain          # downloads, verifies the SHA-256, unpacks
export PATH=~/toolchain/go/bin:~/toolchain/node/bin:$PATH
```

The recipe refuses other Go/Node versions (override: `KINGRES_ALLOW_TOOL_MISMATCH=1`,
and the result is then not "the" release). On WSL put the Linux tools first on
`PATH`: the recipe stops if `go` resolves to a Windows program under `/mnt`.

### The recipe

```
build/build.sh [--rev REV] [--overlay PATH]... [--work DIR] [--prefix /opt/kingres]
```

1. `snapshot.sh` exports `REV` (default `HEAD`) with `git archive`, so uncommitted
   work is never built. `--overlay PATH` copies files from the working tree over
   the snapshot (used to test packaging files that are not committed yet; the build
   info then says `+overlay`). `--src DIR` builds an existing clean tree instead
   (the Dockerfile does).
2. **Server**: `meson setup` as a release build, `ninja`, `meson install` with
   `DESTDIR` into `WORK/stage`, stripped.
3. **Panel**: `npm ci`, `npm run build` (type-check and vite), copied into
   `kingres/control-plane/internal/webui/dist`, the directory `kingresd` embeds.
4. **Go**: `kingresd` and `kingres-verify` with `CGO_ENABLED=0 go build -trimpath
   -buildvcs=false -buildmode=pie -ldflags "-s -w -X main.version=<version> -X main.commit=<commit>"`,
   `GOTOOLCHAIN=local` (no toolchain downloads) and `-mod=readonly` (the locked
   `go.sum` must be complete).
5. Computes the run-time package dependencies with `dpkg-shlibdeps`
   (`WORK/info/shlibs.depends`; the image and the package both use it), checks
   the staged tree (programs, extensions, no assertions, full RELRO and
   `BIND_NOW`, static `kingresd`), writes `share/BUILD-INFO` (commit, versions of
   Go, Node, npm, meson, ninja, gcc and glibc, all meson options) and the
   `SHA256SUMS` of every staged file.

The version is read from `kingres_version` in `meson.build` and embedded in
`kingresd` and `kingres-verify`; the staged tree and the package carry it.
Development files (headers, static libraries, the PGXS kit, ecpg) are pruned: a
future `kingres-dev` package would carry them.

**Meson options and why** (the full list with reasons is in the header of
`build/build.sh`):

| Option | Why |
|---|---|
| `--buildtype release`, `-Dcassert=false` | optimized (-O3), assertions off; developer scripts keep `debugoptimized` + `cassert=true` and are untouched |
| `-Dtap_tests=disabled`, `-Ddocs=disabled` | tests and documentation are not part of the product |
| `-Dicu`, `-Dssl=openssl`, `-Dlz4`, `-Dzstd`, `-Dreadline`, `-Dzlib` `=enabled` | the features an installed server needs: ICU collations, TLS, compression, psql line editing |
| `-Dsystemd=enabled` | `sd_notify` support (the option exists) |
| `-Dlibxml`, `-Duuid=e2fs`, `-Dliburing`, `-Dlibnuma` | SQL/XML, `uuid-ossp` from libuuid, `io_uring` I/O method, NUMA views |
| `-Dsystem_tzdata=/usr/share/zoneinfo` | time zone rules come from the OS `tzdata` package (package depends on it) |
| `-Dllvm`, `-Dgssapi`, `-Dldap`, `-Dpam`, `-Dselinux`, `-Dlibcurl`, `-Dlibxslt`, `-Dnls`, `-Dplperl/plpython/pltcl` `=disabled` | not in the first release: each adds a run-time dependency and attack surface not needed yet |
| `--libdir lib` | libraries in `<prefix>/lib`, the same in every container |
| `-Dc_link_args=['-Wl,-z,relro,-z,now']` | full RELRO and immediate binding |
| rpath (default on) | binaries find `libpq` in `/opt/kingres/lib` without `ld.so` configuration |

Reproducibility: pinned toolchain checksums, `npm ci`, locked Go modules,
`-trimpath`, `SOURCE_DATE_EPOCH` = commit time, fixed archive options. Two builds
of the same commit in the same work directory gave bit-identical packages
(same SHA-256). Build paths do appear in a few server binaries, so a different
`--work` path gives a different `postgres`; the image build always uses `/src`
and `/work`. Network access is needed for `npm ci` and `go mod download` only.

## Container image

```
# from the repository root (or, better, from a clean snapshot):
docker build -f kingres/packaging/docker/Dockerfile -t kingres:0.1.0 .
docker run -d --name kingres --stop-timeout 120 \
  -p 127.0.0.1:8091:8091 -p 127.0.0.1:5432:5432 \
  -v kingres-data:/var/lib/kingres kingres:0.1.0
docker exec kingres cat /var/lib/kingres/setup-token      # the one-time token
```

- Multi-stage. The build stage (Ubuntu 24.04, pinned by digest, plus the pinned
  Go and Node) runs `build.sh`. The final stage installs only the run-time
  packages that `dpkg-shlibdeps` found (plus `tzdata`, `tini`, `ca-certificates`),
  creates the user `kingres` (uid/gid 10001) and copies the staged tree.
- Entry point: `tini`, then `kingres-entrypoint`, which starts `kingresd` and
  plays the role of a service manager (see below). No secret is baked in: the
  token is created at first start in the volume and only its location is logged.
- Settings are environment variables (the image has no config file):
  `KINGRES_LISTEN=0.0.0.0:8091`, `KINGRES_ALLOW_REMOTE_SETUP=true` (a published
  port reaches `kingresd` from the Docker bridge, not from loopback; the setup
  token is still required), `KINGRES_PG_LISTEN=0.0.0.0`, `KINGRES_PG_PORT=5432`,
  `KINGRES_DATA_DIR=/var/lib/kingres`, `KINGRES_PG_BIN_DIR=/opt/kingres/bin`,
  `KINGRES_LOG_FORMAT=json`. Publish the ports to `127.0.0.1` unless you mean
  otherwise. `pg_hba.conf` is managed by `kingresd` and by default accepts no
  network login at all: add rules in the panel's network access page before
  other containers can connect (the test shows the refusal).
- **Volume layout** (`/var/lib/kingres`, one volume): `node-state.json`,
  `setup-token`, `audit.jsonl`, `pki/` (cluster CA and certificates, private),
  `service-credentials`, `instances/1/pgdata` (the database; its log is
  `pgdata/log/`), `run/1` (the Unix socket). Everything the node is lives there.
- **First run**: the container is an uninitialized node. Open `http://127.0.0.1:8091/`,
  enter the token, choose "create cluster". `kingresd` creates the instance, the
  administrator role and the system database and starts the instance; the
  container stays healthy. Log in with `docker exec -e PGPASSWORD=... kingres
  psql -h 127.0.0.1 -U <admin> postgres`.
- **Stop and restart**: `kingresd` starts the instance as an independent
  process and leaves it running when it exits, and a container has no service
  manager. So the entry point forwards `SIGTERM` to `kingresd`, then runs
  `kingresd -stop-instances` (fast shutdown of PostgreSQL), then exits with
  `kingresd`'s status. Give `docker stop` time: `--stop-timeout 120` (compose:
  `stop_grace_period: 120s`). `docker stop`, `docker start`, `docker restart` and
  a new container on the same volume bring the same database back without crash
  recovery (tested). `docker kill` or a host crash is a crash like any other:
  PostgreSQL recovers from its WAL at the next start.
- Health check: `GET /api/v1/readyz` (HTTP 200; an uninitialized node is ready).
- Runs with `--read-only --tmpfs /tmp --cap-drop ALL --security-opt
  no-new-privileges` (the tests do). Sockets are in the volume, never in `/tmp`.
- A bind-mounted data directory must belong to uid 10001. A named volume is
  initialized with the right owner.
- `server-host` (the id of the server in the catalog) defaults to the container's
  host name, which is random unless you pass `--hostname` (compose does).
- The base image is pinned by digest (`ubuntu:24.04@sha256:2198e4c6...`, the
  24.04 image of 2026-11-10) so that a rebuild starts from the same layers; apt
  packages are not pinned, they are that release's security updates. To move
  the base, change the digest in a reviewed commit.

## Development cluster

```
cd kingres/packaging/docker
docker compose up --build -d
./tokens.sh                 # setup token and panel URL of each node
docker compose stop         # shuts the databases down cleanly (120 s grace)
docker compose down --volumes
```

Three nodes `kingres-1..3` (host names and container names), network
`kingres-net`, volumes `kingres-N-data`, panels on `127.0.0.1:8091/8092/8093`
and databases on `127.0.0.1:5432/5433/5434` (`KINGRES_PANEL_PORT_N`,
`KINGRES_PG_PORT_N`, `KINGRES_PANEL_BIND`). The mutual-TLS port of the internal
server-to-server API (`KINGRES_INTERNAL_LISTEN=0.0.0.0:8191`) is switched on inside
the network and not published. **The test of this topology does not join the
nodes into one cluster yet:** it initializes each node as an independent
one-server cluster. `kingresd` can join servers (see
[../control-plane/README.md](../control-plane/README.md)), and the file provides
what joining needs (names that resolve, one network, the internal port, a volume
per node); a join test for this topology is still to be written.

## Debian package

```
build/build.sh --work ~/w            # or any work area
deb/build-deb.sh --work ~/w --lint   # ~/w/deb/kingres_0.1.0-1_amd64.deb
sudo apt install ./kingres_0.1.0-1_amd64.deb
```

### Installed layout

| Path | Content |
|---|---|
| `/opt/kingres/{bin,lib,share}` | the product (server, clients, `kingresd`, `kingres-verify`, extensions, `share/BUILD-INFO`) |
| `/usr/bin/kingres-psql`, `kingres-pg_dump`, `kingres-pg_dumpall`, `kingres-pg_restore`, `kingres-pg_basebackup`, `kingres-pg_isready`, `kingres-pg_config`, `kingresd`, `kingres-verify` | symbolic links into `/opt/kingres/bin` |
| `/usr/lib/systemd/system/kingres.service` | the one service of the node |
| `/etc/kingres/kingresd.json` | settings (conffile, 0644): listen address, data directory, `pg-bin-dir`, `socket-dir`, `pg-port`, `pg-listen`, log settings |
| `/etc/kingres/kingresd.env` | environment and secrets such as `KINGRES_METRICS_TOKEN`, TLS file names (conffile, 0600 root) |
| `/var/lib/kingres` | the node's state (0750 `kingres:kingres`, created by `postinst`, not shipped): `node-state.json`, `setup-token` (0600, only while uninitialized), `audit.jsonl`, `pki/` (cluster CA `ca.key` 0600 and certificates, 0700), `service-credentials` (0600), `instances/1/pgdata` (the database, 0700) |
| `/var/lib/kingres/instances/1/pgdata/log/` | PostgreSQL server log (weekday-rotated `postgresql-Mon.log`...); the manager's log is in the journal: `journalctl -u kingres` |
| `/run/kingres/1` | the instance's Unix socket (created by the unit, 0750) |
| `/usr/share/doc/kingres/` | copyright, changelog, `README.Debian`, dependency lists |

**Command names.** The clients are linked as `kingres-<name>`, not `psql` and
friends: `/usr/bin/psql`, `pg_dump` and the others belong to the distribution's
`postgresql-client-*` packages (and to any other PostgreSQL on the machine). A
prefix cannot collide with them, needs no `update-alternatives`, and makes it
clear which server a tool talks to. The server programs (`postgres`, `initdb`,
`pg_ctl`) are not on the `PATH` at all; `kingresd` uses `pg-bin-dir`
(`/opt/kingres/bin`). The libraries stay in `/opt/kingres/lib` and are found
through RUNPATH, so they never shadow the distribution's `libpq`.

### Service and the two restart modes

`kingres.service` is `Type=notify` (`kingresd` sends `READY=1` once its API
listens, so `apt install` and `systemctl start` return only when the node
answers), runs as `kingres:kingres` (never root), with `KillMode=process`,
`Restart=always`, `TimeoutStopSec=120`, and
`ExecStop=kingresd -config ... -stop-instances`. Because the database is an
independent process, there are two ways to restart:

| What | Command | Effect |
|---|---|---|
| Restart only the manager | `systemctl kill --kill-whom=main --signal=SIGTERM kingres` | `kingresd` exits cleanly, `Restart=always` starts it, it adopts the running instance. The postmaster keeps its PID, connections stay open. `ExecStop` does not run (the unit is not stopped) |
| Restart everything | `systemctl restart kingres` | `ExecStop` shuts the database down (fast shutdown), then everything starts again |
| Stop everything | `systemctl stop kingres` | database shut down, no `postmaster.pid` left |
| After a reboot / `start` | `systemctl start kingres` | `kingresd` starts the instance; the node is a cluster, no wizard |

This was chosen over a second unit because it is the simplest correct arrangement:
one unit, one `systemctl stop` that really stops the database, and a documented
signal for the rare manager-only restart. A crash of `kingresd` (`kill -9`)
behaves like the manager-only restart: the database keeps running. Both are
tested (postmaster PID unchanged, queries answered while the manager is down).
A package upgrade restarts everything (new server binaries must not run under
old processes).

Sandboxing, re-checked against a `kingresd` that runs `initdb`, `pg_ctl` and
`postgres` and writes data, sockets and logs: `NoNewPrivileges`, empty capability
set (no capability in the manager or in the postmaster, checked), `ProtectSystem=strict`
with `ReadWritePaths=/var/lib/kingres` (the runtime directory is writable through
`RuntimeDirectory=kingres kingres/1`, kept across restarts with
`RuntimeDirectoryPreserve=restart`), `ProtectHome`, `PrivateTmp` (safe because
sockets are never in `/tmp`), `PrivateDevices` (`/dev/shm` still works: a parallel
query that uses dynamic shared memory is tested), kernel, clock, hostname and
cgroup protection, `RestrictAddressFamilies` (unix, inet, inet6),
`RestrictNamespaces`, `RestrictRealtime`, `RestrictSUIDSGID`, `LockPersonality`,
`SystemCallFilter=@system-service` (no denied call was seen), `UMask=0027`.
`LimitNOFILE=65536`, `TasksMax=16384`, `OOMScoreAdjust=-100` (the database is
preferred over unrelated services, but not exempt; backends inherit it). Shared
memory: PostgreSQL's main segment is an anonymous mapping, so no
`kernel.shmmax` tuning is needed; huge pages are `try` (used when the host has
them, no capability needed). Left out on purpose so that PostgreSQL keeps
working: `MemoryDenyWriteExecute`, `PrivateNetwork`, `PrivateIPC`, `ProtectProc`.
`systemd-analyze security` rates the unit 1.7 ("OK").

### First run from the package

The service starts at the end of the installation; the node is uninitialized and
the panel listens on `127.0.0.1:8091` only. The installer prints where the token is:

```
sudo cat /var/lib/kingres/setup-token
```

Open `http://127.0.0.1:8091/` on the server (from elsewhere:
`ssh -L 8091:127.0.0.1:8091 server`), enter the token, choose "create cluster",
give the cluster name and the administrator account. `kingresd` creates the
instance under `/var/lib/kingres/instances/1`, the administrator role, the cluster
certificate authority (`/var/lib/kingres/pki`) and the system database, and starts
the instance; the token file is removed. The same through the API (what the
tests do): `POST /api/v1/setup/init-cluster` with the header `X-Setup-Token`, then
poll `GET /api/v1/setup/state`.

To let the panel listen on another address set `KINGRES_LISTEN` in
`/etc/kingres/kingresd.env` (or `listen` in `kingresd.json`) and use TLS
(`KINGRES_TLS_CERT_FILE`, `KINGRES_TLS_KEY_FILE`); other machines may complete
the setup only with `KINGRES_ALLOW_REMOTE_SETUP=true`. The database port is
5432 (`pg-port`); change it in `kingresd.json` before the setup if a PostgreSQL
of the distribution already uses it.

### Connecting to the database

```
kingres-psql -h 127.0.0.1 -p 5432 -U <administrator> postgres
```

Use TCP. The Unix socket is `/run/kingres/1/.s.PGSQL.5432` (mode 0750, group
`kingres`), but `kingres-psql` looks in `/tmp` by default (a compile-time default
of PostgreSQL), so it does not find it without `-h /run/kingres/1`, and "local"
connections use peer authentication that `kingresd` maps to its own OS user only
(the tests confirm that a socket login of the administrator is refused). Network
logins other than from 127.0.0.1 need access rules (panel: network access).
Backup pointers: `kingres-pg_dump -h 127.0.0.1 -U <admin> -Fc shop > shop.dump`
for logical backups; `kingres-pg_basebackup -h 127.0.0.1 -U <role with the
REPLICATION attribute> -D DIR -X stream` for a physical copy (needs a
replication rule in the network access page); the whole state of the node is
`/var/lib/kingres` (stop the service for a cold copy; it includes the CA key and
the service credentials, so treat the copy as a secret). Ledger digests can be
checked offline with `kingres-verify`.

### Upgrade, removal, purge

- **Upgrade**: `apt install ./kingres_<new>.deb`. Configuration changes you made
  are kept (conffiles); `postinst` restarts the service, which restarts the
  database (data in `/var/lib/kingres` is untouched; tested with data present).
  Upgrades across a PostgreSQL major version are a separate procedure and not
  covered yet.
- **Remove** (`apt remove kingres`): the service and the database are stopped
  (cleanly), the program files go; **data, settings and the `kingres` user stay**.
  Installing again brings the same cluster back (tested).
- **Purge** (`apt purge kingres`): additionally deletes `/var/lib/kingres` (every
  database of this node, the certificates, the audit log), `/etc/kingres`, the
  unit's enablement state, the user and the group. This is the standard meaning of
  purge: a deliberate request to discard everything. Take a backup first.

### Lintian

`lintian --fail-on error` passes with no errors and no warnings; information
level tags remain (see the result in `WORK/deb/lintian.txt`):
`binary-has-unneeded-section` (`.comment` of gcc), `hardening-no-fortify-functions`
(8 files),
`package-contains-documentation-outside-usr-share-doc`
(PostgreSQL's time zone set files), `spelling-error-in-binary` (a false positive in
`postgres`).

Overridden, with the reason in `deb/lintian-overrides`: `dir-or-file-in-opt`
(the product is a self-contained `/opt` tree by design),
`custom-library-search-path` (RUNPATH to the private `libpq`),
`library-not-linked-against-libc` and `shared-library-lacks-prerequisites`
(PostgreSQL modules; static-PIE Go programs), `non-standard-file-perm` (the
0600 environment file), `initial-upload-closes-no-bugs` (not a Debian upload).

## Tests of the artifacts

```
test/test-image.sh   [--port 58001]            # build the image, run it, full cluster flow
test/test-compose.sh [--port-base 58100]       # three nodes, each its own cluster
test/test-deb.sh --deb FILE --work WORK        # install ... purge under systemd
```

All three build from a clean snapshot of `HEAD` plus the working-tree
`kingres/packaging` (`--context DIR` overrides). `test/flow.sh` is the shared
"real thing": it creates a cluster through the REST API with the setup token
(`POST /setup/init-cluster`, polling `/setup/state`), signs in, creates a role and
a database with sharding and the ledger, creates two tables with `psql`,
distributes one and enables the ledger on the other through the API, inserts and
reads rows, and generates a ledger digest; `flow_check_*` repeat the reads
after every restart.

- **`test-deb.sh`** starts a clean `ubuntu:24.04` with systemd as PID 1
  (`--privileged`, host cgroup namespace; `policy-rc.d` removed) and checks:
  installation and dependencies; owners and modes; `Type=notify` (the API answers
  the moment `apt` returns); the throw-away server with both extensions; the
  cluster flow; the sandbox (no capabilities in manager and postmaster,
  `NoNewPrivs`, no write outside the allowed directories, parallel query with
  dynamic shared memory, no denied system call); **manager-only restart** (the
  postmaster PID is unchanged and the database answers meanwhile); `kill -9` of
  the manager; `systemctl restart` (the postmaster is new, data stay);
  `systemctl stop` then `start` (cluster back, no wizard, no setup token, no
  crash recovery in the instance log); an upgrade to revision 2 with data
  (database restarted, data and local configuration kept); remove (service and
  database stopped, data kept); reinstall (cluster comes back); purge
  (everything gone, no process left).
- **`test-image.sh`**: the image builds; a container becomes healthy and is
  uninitialized; panel and favicon; non-root, read-only root file system, no
  capabilities; setup token handling; the cluster flow over the published port
  (a non-loopback caller, so `KINGRES_ALLOW_REMOTE_SETUP` is exercised); `docker
  stop` within the grace period with exit code 0 and "database system is shut
  down" in the instance log; `docker start`, `docker restart`, and a new container
  on the same volume, each with the same data and no crash recovery; no secrets
  in the image.
- **`test-compose.sh`**: three nodes healthy and uninitialized on their ports;
  three different tokens; names resolve; each node becomes its own cluster (node 1
  with the full flow); database ports published to loopback; a database login
  across the network is refused (no access rule yet); `docker compose stop`
  (clean) and `start` bring the data back without crash recovery.

## Facts about `kingresd` the packaging depends on

Verified by the tests against the commit built:

1. Settings in `kingresd.json` use only existing keys (`listen`, `data-dir`,
   `pg-bin-dir`, `socket-dir`, `pg-port`, `pg-listen`, `log-format`, `log-level`);
   an unknown key stops the server. The environment overrides the file.
2. `kingresd` sends `READY=1`/`STOPPING=1`, leaves the instance running when it
   exits, adopts it at start, and `-stop-instances` stops it (fast shutdown).
3. `socket-dir` is `/run/kingres`; the instance's socket is `<socket-dir>/1/`.
4. **Defect worked around, needs a change in `kingresd`**: when it starts an
   instance that already exists (after a reboot or `systemctl stop`/`start`), it
   does not create `<socket-dir>/<id>` first; PostgreSQL then fails with "could
   not create lock file /run/kingres/1/.s.PGSQL.5432.lock" and `kingresd` exits.
   The unit works around it with `RuntimeDirectory=kingres kingres/1`, which
   hard-codes instance id 1 (fine for a one-server cluster). The fix belongs in
   the instance manager (`MkdirAll` of the socket directory before `pg_ctl
   start`).
5. `kingres-psql` and the other prefixed programs do not find the instance's
   socket by default (PostgreSQL's compiled-in default is `/tmp`, and the socket is
   in `/run/kingres/1`); administrators connect over TCP (see above). Local socket
   logins are peer-authenticated for `kingresd`'s OS user only.
6. The compose test does not exercise joining yet (see "Development cluster").
7. `systemctl restart` restarts the database too (by design of `ExecStop`); the
   manager-only restart is a signal.

## What is not included yet

- **No Patroni, no etcd, no metrics store** (decided: they arrive with the
  high-availability phase; `kingresd` exposes Prometheus-format metrics itself).
- Joining nodes (the compose file prepares the topology), `kingres-cli`.
- RPM, arm64, Windows; package signing and an apt repository; image signing and
  publishing; SBOM; debug symbols package (`-dbgsym`); a `kingres-dev` package;
  man pages beyond one-page stubs; the licences of every compiled-in Go and npm
  dependency as full texts (only the dependency lists are shipped); NLS, GSSAPI,
  LDAP, PAM, JIT, PL languages.
- Package upgrades are tested for a revision bump only (no database migration,
  no major-version path, no rollback); the instance is restarted on upgrade.
- No automated backup, no PITR archive configuration (pointers above only).

## What was run

Timings and sizes are those of one run on a 24-core WSL2 machine; they vary with
caches and network. The author's run built commit
`28f5360ec690e2a36a7cd8ad706c60cfd8c53dab` plus the then uncommitted
`kingres/packaging` as an overlay. The integrator repeated all three tests on
later commits that add joining to `kingresd` (`ce970614b3` for the image and
compose tests, `2668e604d9` for the package): the image and compose counts were
the same, and the package test gave 156 checks passed, 0 failed.

| Measurement | Value |
|---|---|
| Release recipe, warm Go/npm caches, 24 cores | 28 s |
| Staged tree `/opt/kingres` | 43 MB |
| `kingres_0.1.0-1_amd64.deb` | 11,002,972 bytes (10.5 MiB), xz |
| Image `kingres:test` | 267 MB on disk (Ubuntu 24.04 base 119 MB), 70.8 MB compressed content |
| Image build (cached base layers and apt downloads) | 138 s |
| `docker stop` of a running cluster | 2 s; `docker restart` 1 s |
| Package test (install to purge) | 77 s |
| Image test (build included) | 180 s |
| Compose test | 27 s (images cached) |

| Check | Result |
|---|---|
| `test/test-deb.sh` (clean ubuntu:24.04, systemd) | 150 checks passed, 0 failed |
| `test/test-image.sh` | 79 passed, 0 failed |
| `test/test-compose.sh` | 58 passed, 0 failed |
| `lintian --fail-on error` (2.117) | 0 errors, 0 warnings; 140 information tags (listed above, plus two spelling false positives in `postgres`) |
| `shellcheck -x -P SCRIPTDIR` on every script | clean |
| `actionlint` 1.7.12 (with shellcheck) on `ci.yml` | clean; **the workflow was not executed on GitHub** |

---

Copyright (c) 2026, RooyeKhat Media Co. Released under the PostgreSQL License.
