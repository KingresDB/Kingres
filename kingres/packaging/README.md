<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# KingresDB packaging

"One package: install it and the node has everything"
([architecture](../docs/architecture.md), section "One package, and a panel on
every node"). This directory builds the product from the repository and turns
it into a container image, a three-node development cluster and a Debian
package, and tests each of them.

| Artifact | Where | What it is |
|---|---|---|
| Release build recipe | `build/` | One script (`build.sh`) from a clean source snapshot to a staged installation tree under `/opt/kingres`; builds either PostgreSQL major, which it reads from the source it is given |
| Container image | `docker/Dockerfile`, `docker/build-image.sh` | `kingres:<version>-pg18` or `kingres:<version>-pg19`: minimal Ubuntu 24.04, non-root, `kingresd` as the entry point, health check, starts uninitialized |
| Development cluster | `docker/compose.yaml`, `docker/tokens.sh` | Three nodes of one image (one major), one network, fixed names, published panel ports, volumes |
| Debian package | `deb/` | `kingres_<version>+pg18-<revision>_amd64.deb` or `kingres_<version>+pg19-<revision>_amd64.deb` with the `kingres.service` unit |
| Artifact tests | `test/` | Image, compose and package tests (the package test runs systemd in a container); each is run once per major |
| Where the checks run | developer machines | Every check of this directory runs on a developer's machine (see [Tests of the artifacts](#tests-of-the-artifacts)); no CI workflow of this project exists, runs or is published on GitHub, by decision of the project owner |

## One release per PostgreSQL major

KingresDB is released for PostgreSQL 18 and for PostgreSQL 19
([../docs/dev/releases.md](../docs/dev/releases.md)); the product branches are
`kingres-18` (PostgreSQL 18.7) and `kingres-19` (PostgreSQL 19rc2 until the final
release of 19 exists), with the same KingresDB directories. The packaging builds
either from **the source it is given**; the major is never typed a second time:

- `build/source-info.sh SRC` reads `kingres_version` and the version of the
  `project()` call of `SRC/meson.build` (`18.7`, `19rc2`), derives the major (the
  leading integer, which must be 18 or 19) and the names below. `build.sh`,
  `docker/build-image.sh` and the tests all use it, and `build.sh` checks the
  result against the server it built (`PG_MAJORVERSION`, `postgres --version`).
- The Dockerfile refuses to build when the build arguments it is given
  (`VERSION`, `PG_VERSION`, `PG_MAJOR`, which `build-image.sh` takes from the same
  tree) differ from what the recipe found.

| Thing | PostgreSQL 18 | PostgreSQL 19 |
|---|---|---|
| Debian package | `kingres_0.1.0+pg18-<revision>_amd64.deb` | `kingres_0.1.0+pg19-<revision>_amd64.deb` |
| Image | `kingres:0.1.0-pg18` | `kingres:0.1.0-pg19` |
| `share/BUILD-INFO` | `PostgreSQL: 18.7 (major 18)` | `PostgreSQL: 19rc2 (major 19)` |
| Image labels | `org.kingres.postgresql.major=18`, `.version=18.7`, `org.opencontainers.image.version=0.1.0-pg18` | the same with 19 and `19rc2` |

`kingres:<version>` without a suffix is not produced.

Target: Ubuntu 24.04, amd64. Everything below was run on Windows 11 with WSL2
(Ubuntu 24.04, Docker Engine installed inside the distribution); see
[What was run](#what-was-run).

## What the package contains

- The PostgreSQL based server of the major that was built (18.7 or 19rc2: `postgres`,
  `initdb`, `pg_ctl`, ...), its client programs, all contrib modules, and the five
  KingresDB extensions: `kingres_columnar`, `kingres_ledger`, `kingres_shard`,
  `kingres_sketch` and `kingres_timepart` (`share/BUILD-INFO` lists them; the
  build stops when an extension of the source tree is not in the staged tree).
  `kingres_columnar` needs `shared_preload_libraries = 'kingres_columnar'` to
  write (it registers a WAL resource manager), and the product does **not**
  enable it: without the preload `CREATE EXTENSION` works, a columnar table can
  be created and read, and a write is refused, as its README says. The other
  four need no preload except `kingres_shard` and `kingres_ledger`, which
  `kingresd` preloads.
- `kingresd`: one static Go binary with the web panel embedded, and
  `kingres-watchdog`, the lease watchdog next to it.
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

`REV` is a commit or a product branch: `--rev kingres-18` builds PostgreSQL 18.7
with KingresDB, `--rev kingres-19` builds PostgreSQL 19rc2 with KingresDB. The
recipe is the same; the major is the one of the tree (`build/source-info.sh`).
Always build from a branch or commit, never from a working tree that holds
unfinished work.

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
5. Checks that the tree says the same as the server it built (major and
   version), and that every `contrib/kingres_*` extension (control file, install
   script for its default version, and the library when the control file names
   a module) is in the staged tree. It copies the text of the AGPL and the two
   dependency lists to `WORK/info/doc`, from which the package and the image
   take their documentation.
6. Computes the run-time package dependencies with `dpkg-shlibdeps`
   (`WORK/info/shlibs.depends`; the image and the package both use it), checks
   the staged tree (programs, no assertions, full RELRO and `BIND_NOW`, static
   `kingresd`), writes `share/BUILD-INFO` (the KingresDB version, the PostgreSQL
   version and major, the extensions, the commit, versions of Go, Node, npm,
   meson, ninja, gcc and glibc, all meson options), the files `version`,
   `pg-version`, `pg-major` and `extensions` of `WORK/info`, and the `SHA256SUMS`
   of every staged file.

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
# one image per major, from a product branch (a clean snapshot is made):
kingres/packaging/docker/build-image.sh --rev kingres-18     # kingres:0.1.0-pg18
kingres/packaging/docker/build-image.sh --rev kingres-19     # kingres:0.1.0-pg19
docker run -d --name kingres --stop-timeout 120 \
  -p 127.0.0.1:8091:8091 -p 127.0.0.1:5432:5432 \
  -v kingres-data:/var/lib/kingres kingres:0.1.0-pg18
docker exec kingres cat /var/lib/kingres/setup-token      # the one-time token
```

`build-image.sh [--context DIR | --rev REV] [--tag TAG]` reads the version, the
PostgreSQL version and the major from the context and passes them to the
Dockerfile, which compares them with what the recipe found (a plain `docker
build` without them fails with a message). Use a volume with one major only: a
data directory of one major does not start under the other.

- Labels: `org.opencontainers.image.version` (`0.1.0-pg18`),
  `org.opencontainers.image.licenses` (`AGPL-3.0-only AND PostgreSQL`),
  `org.opencontainers.image.revision`, `org.kingres.version`,
  `org.kingres.postgresql.version` (`18.7`, `19rc2`) and
  `org.kingres.postgresql.major` (`18`, `19`). The image carries the licence
  documents in `/usr/share/doc/kingres/` (`copyright`, `AGPL-3.0.txt`, the two
  dependency lists), the same as the package.
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
./build-image.sh --rev kingres-19                  # kingres:0.1.0-pg19
export KINGRES_IMAGE=kingres:0.1.0-pg19            # required: the image of one major
docker compose up -d
./tokens.sh                 # setup token and panel URL of each node
docker compose stop         # shuts the databases down cleanly (120 s grace)
docker compose down --volumes
```

The compose file takes the image from `KINGRES_IMAGE` and has no default: all
three nodes (all servers of a cluster) run one major, so it is chosen on
purpose. It does not build; `build-image.sh` does.

Three nodes `kingres-1..3` (host names and container names), network
`kingres-net`, volumes `kingres-N-data`, panels on `127.0.0.1:8091/8092/8093`
and databases on `127.0.0.1:5432/5433/5434` (`KINGRES_PANEL_PORT_N`,
`KINGRES_PG_PORT_N`, `KINGRES_PANEL_BIND`, `KINGRES_IMAGE`).

### Making the three nodes one cluster

Every node starts uninitialized with its own setup token. What joining needs is
set in `compose.yaml` (see [../control-plane/README.md](../control-plane/README.md),
"Running several servers"):

| Setting | Value here | Why |
|---|---|---|
| `KINGRES_INTERNAL_LISTEN` | `0.0.0.0:8092` | the mutual-TLS port of the server-to-server API, **the port above the REST port** (`KINGRES_LISTEN` is `0.0.0.0:8091`): a server is known to the others by its REST address and the internal API is looked up at the next port. Not published to the host; the nodes reach it over `kingres-net`. (The file said `8191` before the join test; `kingresd` expects the port above the REST port and warns otherwise.) |
| `KINGRES_SERVER_HOST` | `kingres-N` | the server id: its certificate names it, the other nodes and the sharding extension connect to it. It is the container host name and resolves on the network |
| `KINGRES_PG_LISTEN` | `0.0.0.0` (image default) | the other nodes' instances connect to the database port |
| `KINGRES_ALLOW_REMOTE_SETUP` | `true` (image default) | the setup calls reach the panel port through the published port |
| volume `kingres-N-data` | `/var/lib/kingres` | key, certificates, instance; a node keeps its membership across `docker compose restart` and `stop`/`start` |

There are two ways to make a node a member (both are in `test/test-compose.sh`):

1. **The cluster adds the node** (`addServer`). On kingres-1, signed in as
   `kingres_ops` or better, with the setup token of the new node and the
   SHA-256 fingerprint of the new node's self-signed internal certificate in
   `X-Kingres-Pin-Sha256` (it is in the log line "internal API bootstrap
   certificate" and is the SHA-256 of `/var/lib/kingres/pki/bootstrap.crt`):
   ```
   POST http://127.0.0.1:8091/api/v1/servers
   {"address":"kingres-2:8091","setupToken":"<token of kingres-2>"}
   ```
   The answer is the task `server.add` (`GET /tasks/{id}`).
2. **The node joins** (`joinCluster`). On the new node, with its own setup
   token, the address of any member, the administrator of the cluster and the
   fingerprint of the member's cluster certificate (`pki/server.crt` of
   kingres-1) pinned:
   ```
   POST http://127.0.0.1:8093/api/v1/setup/join-cluster
   {"existingNodeAddress":"kingres-1:8091","admin":{"username":"...","password":"..."},
    "dataPaths":{"dataDirectory":"/var/lib/kingres"},"memoryQuota":{"totalMegabytes":1024}}
   ```
   Progress is `setupTask` of `GET /setup/state` on the new node. A wrong pin
   leaves the node uninitialized with `setupTask.state = failed` and the same
   setup token.

A session is held in the memory of the server that issued it: a bearer token
of one node is not used on another, sign in on each node (same administrator).
DDL of a sharded database (`create table`) runs on kingres-1, the map group
primary; the table is distributed through the API (any node), and rows can be
written and read on any node (one row per `INSERT`: a statement that spans
shard groups is not supported yet).

## Debian package

```
build/build.sh --rev kingres-18 --work ~/w18         # or kingres-19, and any work area
deb/build-deb.sh --work ~/w18 --lint                 # ~/w18/deb/kingres_0.1.0+pg18-1_amd64.deb
sudo apt install ./kingres_0.1.0+pg18-1_amd64.deb
```

### One package name, the major in the version

Both majors are the package `kingres`; the version carries the major:
`0.1.0+pg18-1` and `0.1.0+pg19-1` (`<version>+pg<major>-<revision>`; the
changelog entry and the control file say it too, and the description names the
PostgreSQL version). **Only one of them can be installed on a system**, because
dpkg allows one package of a name, and all servers of a cluster run one major.
That is the rule; what keeps the package manager to it is the following.

`+pg19` sorts after `+pg18`, so `apt install ./kingres_0.1.0+pg19-1_amd64.deb` (or
any repository that offers both) would treat the one as an upgrade of the other.
That would replace the programs under the data directory of a cluster of the
other major, which cannot run it, and moving a cluster between majors is not
designed ([releases.md](../docs/dev/releases.md)). The **`preinst` script of the
package refuses it, before anything is unpacked**: it knows its own major (put in
by `build-deb.sh`) and exits with an error and this message when either

1. the installed, or removed-but-not-purged, version has another major (a version
   without `+pgNN`, from before the major was in the name, counts as 18), or
2. a data directory `/var/lib/kingres/instances/*/pgdata` has a `PG_VERSION` of
   another major, whatever the state of the package (the data stays after
   `apt remove`).

```
kingres: refusing to install KingresDB for PostgreSQL 19.
Version 0.1.0+pg18-1 (PostgreSQL 18) is installed, or was removed without purge.

KingresDB cannot be moved between PostgreSQL majors by the package manager: ...
```

Nothing has been changed when it refuses: the old package, its service and its data
keep working (tested: same version, same programs, same postmaster process, data
readable). The way to the other major is a purge of the old package (which deletes
the node's data: take a logical backup first) or another machine. `apt upgrade` on a
system with a repository that offers both would report the refusal for each upgrade,
so offer one major per package source. The test does the downgrade with
`--allow-downgrades`, because without it apt stops a 19 to 18 downgrade itself
before the package is looked at.

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
| `/run/kingres/1` | the instance's Unix socket (`/run/kingres` is created by the unit, 0750; the directory of each instance by `kingresd`, 0700) |
| `/usr/share/doc/kingres/` | `copyright` (machine-readable), `AGPL-3.0.txt` (the full licence text), changelog, `README.Debian`, dependency lists |

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
`RuntimeDirectory=kingres`, kept across restarts with
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

Use TCP. The Unix socket is `/run/kingres/1/.s.PGSQL.5432` (in a directory only
the service user can enter), but `kingres-psql` looks in `/tmp` by default (a compile-time default
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
  An upgrade from one PostgreSQL major to the other (`+pg18` to `+pg19` or back) is
  **refused** by the package, as above: the procedure to move a cluster does not
  exist yet.
- **Remove** (`apt remove kingres`): the service and the database are stopped
  (cleanly), the program files go; **data, settings and the `kingres` user stay**.
  Installing again brings the same cluster back (tested).
- **Purge** (`apt purge kingres`): additionally deletes `/var/lib/kingres` (every
  database of this node, the certificates, the audit log), `/etc/kingres`, the
  unit's enablement state, the user and the group. This is the standard meaning of
  purge: a deliberate request to discard everything. Take a backup first.

### Lintian

`lintian --fail-on error` passes with no errors and no warnings on both packages;
information level tags remain (see the result in `WORK/deb/lintian.txt`). The
licence change (a machine-readable `copyright` with several licences, the AGPL
text in the documentation directory, the `preinst`) produced **no new tag**: the
copyright stanzas, their licence names and the references pass lintian's
`debian/copyright` checks. The tags that changed are only these: the warning
`shared-library-lacks-prerequisites` for `kingres-watchdog` (now overridden), and
the counts of the information tags (the package has more files per major):
`binary-has-unneeded-section` (`.comment` of gcc), `hardening-no-fortify-functions`
(8 files),
`package-contains-documentation-outside-usr-share-doc`
(PostgreSQL's time zone set files), `spelling-error-in-binary` (a false positive in
`postgres`).

Overridden, with the reason in `deb/lintian-overrides`: `dir-or-file-in-opt`
(the product is a self-contained `/opt` tree by design),
`custom-library-search-path` (RUNPATH to the private `libpq`),
`library-not-linked-against-libc` and `shared-library-lacks-prerequisites`
(PostgreSQL modules; static-PIE Go programs, now also `kingres-watchdog`, which
had been added to the package without the override and so produced one warning
before this change), `non-standard-file-perm` (the
0600 environment file), `initial-upload-closes-no-bugs` (not a Debian upload).

## Licences

[ADR 0024](../docs/adr/0024-agpl-for-kingresdb-code.md) and [COPYRIGHT](../COPYRIGHT):
KingresDB's own code is **AGPL-3.0-only**, PostgreSQL's code stays under the
**PostgreSQL License**. What the artifacts say:

| Where | What |
|---|---|
| `deb/copyright` (installed as `/usr/share/doc/kingres/copyright`) | Machine-readable (DEP-5). `Files: *` is PostgreSQL's (PostgreSQL License). The AGPL-3.0-only stanzas are `kingresd`, `kingres-verify`, `kingres-watchdog` (the first two also carry the third-party Go modules and npm packages compiled into them, under Expat and BSD-3-Clause, with the lists in `DEPENDENCIES.*.md`), the extension libraries and `share/postgresql/extension/kingres_*`, the settings files, the unit, `README.Debian`, the man pages, `BUILD-INFO`. `kingres_shard.so` is `AGPL-3.0-only and PostgreSQL` (one file, `src/shippable.c`, is adapted from PostgreSQL and carries both). The text of the AGPL is not in the file, it is referred to |
| `/usr/share/doc/kingres/AGPL-3.0.txt` | The full text of the GNU Affero General Public License, version 3 (taken from `LICENSE` of the source tree, which the build checks). Debian and Ubuntu have **no** AGPL file in `/usr/share/common-licenses` (checked on the `ubuntu:24.04` image used for the tests: it has Apache-2.0, Artistic, BSD, CC0-1.0, GFDL*, GPL*, LGPL*, MPL-1.1 and MPL-2.0 only), so the package carries it |
| Image label `org.opencontainers.image.licenses` | `AGPL-3.0-only AND PostgreSQL`; the image has the same three documents in `/usr/share/doc/kingres/` |
| `README.Debian`, `man` pages | the same statement in prose; the man pages and the changelog name the PostgreSQL major |

Third-party licences of the compiled-in Go modules and npm packages are not
shipped as full texts, only as lists (as before); Expat and BSD-3-Clause are quoted
once in the copyright file.

## Tests of the artifacts

```
test/test-image.sh   --rev kingres-19 [--port 58790]       # build the image, run it, full cluster flow
test/test-compose.sh --rev kingres-19 [--port-base 58700]  # three nodes joined into one cluster
test/test-deb.sh --deb FILE --other-deb FILE --work WORK   # install ... purge under systemd
```

Each test is run once per major, on the source of that major. The image and
compose tests build from a clean snapshot of `--rev` (default `HEAD`, a product
branch for a release) plus the working-tree `kingres/packaging`, so that packaging
changes that are not committed yet are tested (`--context DIR` overrides; the
build info then says `+overlay`); the PostgreSQL major they expect is the one
of that tree, and the package test takes it from the version of the package it is
given (`0.1.0+pg18-1`) and checks that the server, `BUILD-INFO` and the labels
agree. `test-deb.sh` also takes the package of the **other** major
(`--other-deb`) for the refusal tests below (`--without-other-major` skips them
and says so). The tests need no network port except the one given, chosen from the
range that is free on the machine (`--port` of the image test, `--port-base` of the
compose test, which uses `N+1..3` and `N+11..13`). `test/flow.sh` is the shared
"real thing": it creates a cluster through the REST API with the setup token
(`POST /setup/init-cluster`, polling `/setup/state`), signs in, creates a role and
a database with sharding and the ledger, creates two tables with `psql`,
distributes one and enables the ledger on the other through the API, inserts and
reads rows, and generates a ledger digest; `flow_check_*` repeat the reads
after every restart.

- **`test-deb.sh`** starts a clean `ubuntu:24.04` with systemd as PID 1
  (`--privileged`, host cgroup namespace; `policy-rc.d` removed) and checks:
  installation and dependencies; the version carries the major and the server,
  `BUILD-INFO` and `kingres-psql` agree with it; the five extensions are
  installed (control file, install script, library) and `BUILD-INFO` lists
  them; the licence files (the full AGPL text, a machine-readable `copyright`
  that puts KingresDB's code under the AGPL and PostgreSQL's under the
  PostgreSQL License, the dependency lists); owners and modes; `Type=notify` (the API answers
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
  (everything gone, no process left). In the throw-away server (`pg-smoke.sh`) and
  in the running node every extension is created in a database
  (`kingres_columnar` without the preload: `CREATE EXTENSION` works, a columnar
  table can be created and read and a write is refused; with the preload, as an
  experiment and not the default, a write works). **The other major** (needs
  `--other-deb`): with the cluster running and data present, the package of the
  other major is refused by `apt-get install` and by `dpkg -i` with the message of
  the `preinst`, and nothing changed (version, programs, service, postmaster PID,
  data); after `apt remove` (data kept) it is refused again; with no package and
  a leftover data directory of this major it is refused; and after a purge it
  installs and starts (and the package of the first major is refused in turn).
- **`test-image.sh`**: the image builds under the name of its major; its labels
  (major, PostgreSQL version, KingresDB version, `AGPL-3.0-only AND PostgreSQL`)
  and `postgres --version` agree with the source tree; the five extensions are
  installed and created in a throw-away server and in the running node; the
  licence files are in the image; a container becomes healthy and is
  uninitialized; panel and favicon; non-root, read-only root file system, no
  capabilities; setup token handling; the cluster flow over the published port
  (a non-loopback caller, so `KINGRES_ALLOW_REMOTE_SETUP` is exercised); `docker
  stop` within the grace period with exit code 0 and "database system is shut
  down" in the instance log; `docker start`, `docker restart`, and a new container
  on the same volume, each with the same data and no crash recovery; no secrets
  in the image.
- **`test-compose.sh`**: the image of the major (labels checked); three nodes
  healthy and uninitialized on their ports, each running that PostgreSQL and
  holding the five extensions;
  three different tokens; the fingerprint in the log is the SHA-256 of
  `pki/bootstrap.crt`. Then one cluster of three servers, through the REST API
  only: kingres-1 is initialized (the full flow of `flow.sh`, with a role, a
  sharded database with the ledger and rows); kingres-2 is added by
  `addServer` on kingres-1 and kingres-3 joins by `joinCluster`, both with the
  certificate fingerprint pinned (a join with a wrong pin is refused first and
  leaves the node uninitialized with the same setup token). Checks: `listServers`
  on every node shows three healthy servers (`live` or `pendingRebalance`), one
  manager leader, the pin recorded; the administrator signs in on every node (a
  session is per server: the token of kingres-1 is only reported on the others);
  the role, the database and the distributed table of kingres-1 exist on the new
  servers; a sharded database `mesh` created through kingres-2 is ready on all
  three; a table created on kingres-1, distributed through kingres-3, written
  row by row on kingres-3, counted on all three; `docker compose restart` of
  kingres-2 (a follower) and, separately, a stop with rows written on kingres-3
  meanwhile (a row whose bucket is on the stopped server is refused, the others
  must be there after the start); stop and start of all nodes (exit code 0, data
  back, no crash recovery: when an instance does come back through crash
  recovery the test prints the node and what its instance, kingresd and the
  watchdog logged); ports bound to loopback; tear down. No check is a known
  failure any more: the servers exchange heartbeats, so a stopped follower is
  no longer listed as healthy. `--keep` leaves the nodes running for a look.

## Facts about `kingresd` the packaging depends on

Verified by the tests against the commit built:

1. Settings in `kingresd.json` use only existing keys (`listen`, `data-dir`,
   `pg-bin-dir`, `socket-dir`, `pg-port`, `pg-listen`, `log-format`, `log-level`);
   an unknown key stops the server. The environment overrides the file.
2. `kingresd` sends `READY=1`/`STOPPING=1`, leaves the instance running when it
   exits, adopts it at start, and `-stop-instances` stops it (fast shutdown).
3. `socket-dir` is `/run/kingres`; the instance's socket is `<socket-dir>/1/`.
4. `kingresd` creates `<socket-dir>/<id>` before every start of an instance,
   so the cluster comes back after a reboot, when `/run` is empty (the package
   test stops and starts the service, which removes the runtime directory). An
   earlier build did not, and the unit then created `/run/kingres/1` itself.
5. `kingres-psql` and the other prefixed programs do not find the instance's
   socket by default (PostgreSQL's compiled-in default is `/tmp`, and the socket is
   in `/run/kingres/1`); administrators connect over TCP (see above). Local socket
   logins are peer-authenticated for `kingresd`'s OS user only.
6. A cluster of the three compose nodes forms through `addServer` and `joinCluster` with
   pinned fingerprints (see "Development cluster"); the internal API must be on the port
   above the REST port.
7. `systemctl restart` restarts the database too (by design of `ExecStop`); the
   manager-only restart is a signal.
8. `kingresd` of this release only accepts the programs of PostgreSQL 18 (`VerifyBinaries`), so on PostgreSQL 19 the package, the image and the compose nodes install and build but their service does not become ready; see "Defects of the product found by these tests".

## What is not included yet

- **No Patroni, no etcd, no metrics store** (decided: they arrive with the
  high-availability phase; `kingresd` exposes Prometheus-format metrics itself).
- `kingres-cli`; removing or leaving servers, rebalancing and failover in the compose test
  (not written; `kingresd` has no heartbeat or failover yet).
- RPM, arm64, Windows; package signing and an apt repository; image signing and
  publishing; SBOM; debug symbols package (`-dbgsym`); a `kingres-dev` package;
  man pages beyond one-page stubs; the licences of every compiled-in Go and npm
  dependency as full texts (only the dependency lists are shipped); NLS, GSSAPI,
  LDAP, PAM, JIT, PL languages.
- Package upgrades are tested for a revision bump only (no database migration,
  no major-version path, no rollback); the instance is restarted on upgrade.
- No automated backup, no PITR archive configuration (pointers above only).

## What was run

The release builds and all tests below were run on 2026-11-15 on a 24-core WSL2
machine (Windows 11, Ubuntu 24.04, Docker Engine in the distribution), each major
from **a committed revision** with the working-tree `kingres/packaging` as an
overlay (`+overlay` in `BUILD-INFO`); a working tree with unfinished work was
never built:

| Major | Revision built | PostgreSQL | Artifacts |
|---|---|---|---|
| 18 | `kingres-18` at `1a7a04f3c2` | 18.7 | `kingres_0.1.0+pg18-1_amd64.deb` (12,125,000 bytes), image `kingres:0.1.0-pg18` |
| 19 | `kingres-19` at `19f94d5279` (the sync from `1a7a04f3c2`) | 19rc2 | `kingres_0.1.0+pg19-1_amd64.deb` (12,351,604 bytes), image `kingres:0.1.0-pg19` |

The release recipe takes 5 to 7 minutes cold for either major, the image build 78 s
(18) and 94 s (19) with warm caches, the content size of the image is 73 MB.

| Check | PostgreSQL 18 | PostgreSQL 19 |
|---|---|---|
| `test/test-deb.sh` (clean ubuntu:24.04, systemd; the package of the other major as `--other-deb`) | 234 passed, 2 failed, 1 known | 102 passed, 7 failed, 3 groups not run |
| `test/test-image.sh` | 129 passed, 0 failed, 1 known | 44 passed, 1 failed, 2 groups not run |
| `test/test-compose.sh` (three nodes joined into one cluster) | 162 passed, 0 failed | 19 passed, 12 failed, the cluster not run |
| `lintian --fail-on error` (2.117) | 0 errors, 0 warnings, 143 information tags | 0 errors, 0 warnings, 145 information tags |
| `shellcheck -x -P SCRIPTDIR` on every script (`build/*.sh`, `deb/build-deb.sh`, the maintainer scripts `preinst` `postinst` `prerm` `postrm`, `docker/*.sh`, `test/*.sh`) | clean | clean (the same scripts) |

The compose test counts 162 checks where it counted 194 before: the extension files
are now checked once, in the image, not on each of the three nodes.

**Not failures of the packaging.** Every failure on PostgreSQL 19, and the two on
18, have one cause, a defect of `kingresd` (next section): `kingresd` of this
release refuses to start with the programs of PostgreSQL 19. The two on 18 are the
checks "API answers" and "setup state" in the step of the package test that installs
the package of PostgreSQL 19 after a purge: the package installs (and `kingres-psql`
is that major's) but its service never becomes ready.

- **Known (marked)**: `kingres_shard` cannot be created in a database after
  `kingres_columnar` (next section).
- **19, package test**: the 7 checks that failed are the ones after the install that
  need `kingresd` to run (the service stays `activating`: it exits at start and
  `Restart=always` starts it again): service active, API answers, process user, setup
  state, panel index page, favicon, panel on loopback. Not run: the cluster flow, the
  sandbox, the restarts, stop and start, the upgrade to revision 2, the API and the
  data after the refusal, the reinstall. **What ran and passed on 19** (102 checks):
  installation and ownership, the version and the server (`postgres (PostgreSQL)
  19rc2`, `BUILD-INFO`, `kingres-psql`), the five extensions installed and created in
  a throw-away server (and the columnar behaviour with and without the preload), the
  licence files, the command names and man pages, the refusal of the package of
  PostgreSQL 18 (`apt-get install`, `dpkg -i`, after `apt remove`, over a leftover
  data directory), the install of the package of PostgreSQL 18 after the purge (that
  one runs), remove and purge.
- **19, image test**: the image builds with its labels; 44 checks passed (labels,
  `postgres --version`, `BUILD-INFO`, the five extensions: files and a throw-away
  server, the licence files, no secrets). The container never becomes healthy. Not
  run: panel, identity, token, restart, the cluster, `docker stop/start/restart`, a new
  container on the volume.
- **19, compose test**: the image labels and the extension files pass; the three
  nodes never become healthy (4 failed checks each); the tokens, the cluster and all
  that follows are not run.

**What would run on 19 once `kingresd` accepts it (diagnostic, not a result of the
packaging).** To see what comes after, the same tests were run on a scratch copy of
`kingres-19` in which that one check of `kingresd` was widened (the copy was not
committed and is not in the repository). The image test passed completely (129
passed, 0 failed, 1 known: the same as on 18), the compose test of three nodes passed
(162 passed, 0 failed) and the package test passed (236 passed, 0 failed, 1 known).
So PostgreSQL 19rc2 runs under `kingresd` with the tested flows (cluster, sharded
database, ledger, restarts, stop and start, the join of two servers) once that check
is fixed. That copy was built from `kingres-19` at `757b5044c4`, one revision before
the one in the table.

| Package test, the refusal of the other major | PostgreSQL 18 | PostgreSQL 19 |
|---|---|---|
| refused with data present (`apt-get install --allow-downgrades`, `dpkg -i`): same version, programs, postmaster PID, data | passed | passed (the checks of the API and the data are not run) |
| refused after `apt remove`; refused over a leftover data directory; installs after a purge | passed | passed |

Earlier runs, for the record: the integrator repeated the package test on commit
`0e35d07ff5` (156 checks, 0 failed), and the first runs of the author gave 150
(package), 79 (image) and 140 (compose, commit `d546dcc06d`), before the tests were
extended for two majors, the five extensions and the licences.

## Defects of the product found by these tests (not fixed here)

1. **`kingresd` refuses PostgreSQL 19** (this stops every test of 19 that needs a
   running node). `kingres/control-plane/internal/cluster/binaries.go`, function
   `VerifyBinaries`: every program must print `KingresDB` or contain the text
   `(PostgreSQL) 18` in `--version`. The programs print `postgres (PostgreSQL)
   19rc2` (they never print `KingresDB`, only `version()` does), so the check fails
   at the first start and `kingresd` exits with `pg-bin-dir "/opt/kingres/bin":
   postgres reports "postgres (PostgreSQL) 19rc2", which is not a KingresDB 18
   build`. Reproduction: start `kingresd` with `pg-bin-dir` pointing at a build of
   `kingres-19`. The error text and the comment of the function also say 18, and
   `control-plane/README.md` documents "a directory that is not a KingresDB 18 build
   stops the server". PostgreSQL 19 changed nothing here: the check was written for
   18 only.
2. **`kingres_shard` cannot be created after `kingres_columnar` or
   `kingres_timepart` in one database** (on 18 and on 19). The install script of
   `kingres_shard` (`contrib/kingres_shard/kingres_shard--0.5.sql`, line 18, and the
   scripts of the earlier versions) runs `CREATE SCHEMA kingres;` without
   `IF NOT EXISTS`, and the control files of `kingres_columnar` and
   `kingres_timepart` say `schema = kingres`, which makes `CREATE EXTENSION` create
   the schema first: `ERROR: schema "kingres" already exists`. The tests create
   `kingres_shard` first (as the product does) and mark the other order as a known
   failure.
3. **`kingres_columnar` without the preload is stricter than its README says**: the
   README says "without the preload the extension still loads and reads, and refuses
   to write". `CREATE EXTENSION` works, but `CREATE TABLE ... USING columnar` is
   already refused (`kingres_columnar must be listed in shared_preload_libraries to
   write columnar tables`), so a table can only exist if it was made with the
   preload; such a table is read, and a write is refused, once the preload is taken
   away again (all tested). The extension is in a fix round; this is a point for its
   README.
4. **Licence headers** (fixed since): at the revisions these tests were first run
   on, 48 files of `kingres/control-plane`, `kingres/web` and `kingres/docs` still
   carried the identifier of the PostgreSQL License in their header, against ADR 0024
   and the `copyright` file of the package. They carry AGPL-3.0-only as of commit
   `5555b252a0`.

The integrator ran the PostgreSQL 18 column again on commit `5555b252a0` with this
directory on top: image test 129 passed, 0 failed, 1 known; compose test 162 passed,
0 failed; package built, lintian without errors, package test 202 passed, 0 failed,
1 known, without the package of the other major (the refusal tests were not run in
that run). Defects 1 and 2 are being fixed in the control plane and in
`kingres_shard`; until defect 1 is fixed nothing of PostgreSQL 19 runs under
`kingresd`.

---

Copyright (c) 2026, RooyeKhat Media Co. Released under the GNU Affero General Public License, version 3 (AGPL-3.0-only); PostgreSQL's own files stay under the PostgreSQL License.
