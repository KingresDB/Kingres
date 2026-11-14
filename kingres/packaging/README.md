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
   -buildvcs=false -buildmode=pie -ldflags "-s -w -X main.version=<version>"`,
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
docker run -d --name kingres -p 127.0.0.1:8091:8091 -v kingres-data:/var/lib/kingres kingres:0.1.0
docker exec kingres cat /var/lib/kingres/setup-token      # the one-time token
```

- Multi-stage. The build stage (Ubuntu 24.04, pinned by digest, plus the pinned
  Go and Node) runs `build.sh`. The final stage installs only the run-time
  packages that `dpkg-shlibdeps` found (plus `tzdata`, `tini`, `ca-certificates`),
  creates the user `kingres` (uid/gid 10001) and copies the staged tree.
- Entry point: `tini` then `kingres-entrypoint` (prints where the setup token
  is, then `exec`s `kingresd`). No secret is baked in: the token is created at
  first start in the volume (`/var/lib/kingres`, mode 0600 inside a 0750
  directory) and only its location is logged.
- `kingresd` listens on `0.0.0.0:8091` in the container (a published port must
  reach it); publish it to `127.0.0.1` unless you mean otherwise. The node is
  uninitialized, the wizard asks for the token.
- Health check: `GET /api/v1/readyz` (HTTP 200; an uninitialized node is ready).
- Runs with `--read-only --tmpfs /tmp --cap-drop ALL --security-opt
  no-new-privileges` (the tests do).
- A bind-mounted data directory must belong to uid 10001. A named volume is
  initialized with the right owner.
- The base image is pinned by digest (`ubuntu:24.04@sha256:2198e4c6...`, the
  24.04 image of 2026-11-10) so that a rebuild starts from the same layers; apt
  packages are not pinned, they are that release's security updates. To move
  the base, change the digest in a reviewed commit.

## Development cluster

```
cd kingres/packaging/docker
docker compose up --build -d
./tokens.sh                 # setup token and panel URL of each node
docker compose down --volumes
```

Three nodes `kingres-1..3` (host names and container names), network
`kingres-net`, volumes `kingres-N-data`, panels on `127.0.0.1:8091/8092/8093`
(`KINGRES_PANEL_PORT_N`, `KINGRES_PANEL_BIND`). **Joining nodes into one cluster is
not implemented in `kingresd` yet**: today these are three independent
uninitialized nodes; the file prepares the topology (names, network, ports,
volumes) that the join feature will use.

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
| `/etc/kingres/kingresd.json` | settings (conffile, 0644) |
| `/etc/kingres/kingresd.env` | environment and secrets such as `KINGRES_PG_DSN` (conffile, 0600 root) |
| `/var/lib/kingres` | node state, setup token, audit log, database instances (0750 `kingres:kingres`, created by `postinst`, not shipped) |
| `/run/kingres` | runtime directory of the unit (sockets) |
| `/usr/share/doc/kingres/` | copyright, changelog, `README.Debian`, dependency lists |

**Command names.** The clients are linked as `kingres-<name>`, not `psql` and
friends: `/usr/bin/psql`, `pg_dump` and the others belong to the distribution's
`postgresql-client-*` packages (and to any other PostgreSQL on the machine). A
prefix cannot collide with them, needs no `update-alternatives`, and makes it
clear which server a tool talks to. The server programs (`postgres`, `initdb`,
`pg_ctl`) are not on the `PATH` at all; `kingresd` uses `/opt/kingres/bin`
(`KINGRES_PG_BIN_DIR`, see assumptions). The libraries stay in `/opt/kingres/lib`
and are found through RUNPATH, so they never shadow the distribution's `libpq`.

### Service

`kingres.service` runs `kingresd -config /etc/kingres/kingresd.json` as
`kingres:kingres` (no root at any time), `Restart=on-failure`, `TimeoutStopSec=120`.
Sandboxing: `NoNewPrivileges`, empty capability set, `ProtectSystem=strict` with
`ReadWritePaths=/var/lib/kingres`, `ProtectHome`, `PrivateTmp`, `PrivateDevices`,
kernel, clock, hostname and cgroup protection, `RestrictAddressFamilies`
(unix, inet, inet6), `RestrictNamespaces`, `RestrictRealtime`, `RestrictSUIDSGID`,
`LockPersonality`, `SystemCallFilter=@system-service`, `UMask=0027`. Left out on
purpose so that PostgreSQL keeps working: `MemoryDenyWriteExecute`,
`PrivateNetwork`, `ProtectProc`. `systemd-analyze security` rates the unit 1.7
("OK").

### First run from the package

The service starts at the end of the installation; the node is uninitialized and
listens on `127.0.0.1:8091` only. The installer prints where the token is:

```
sudo cat /var/lib/kingres/setup-token
```

Open `http://127.0.0.1:8091/` on the server (from elsewhere:
`ssh -L 8091:127.0.0.1:8091 server`) and enter the token in the wizard. To
listen on another address set `listen` in `/etc/kingres/kingresd.json` (or
`KINGRES_LISTEN` in `kingresd.env`) and `systemctl restart kingres`; configure TLS
(`tls-cert-file`, `tls-key-file`) when you do.

### Upgrade, removal, purge

- **Upgrade**: `apt install ./kingres_<new>.deb`. Configuration changes you made
  are kept (conffiles); the service is restarted by `postinst`; state in
  `/var/lib/kingres` is untouched. Upgrades across a PostgreSQL major version are
  a separate procedure and not covered yet.
- **Remove** (`apt remove kingres`): the service is stopped and disabled, the
  program files go; **data, settings and the `kingres` user stay**.
- **Purge** (`apt purge kingres`): additionally deletes `/var/lib/kingres` (every
  database of this node), `/etc/kingres`, the unit's enablement state, the user
  and the group. This is the standard meaning of purge: a deliberate request to
  discard everything. Take a backup first.

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
test/test-image.sh   [--port 58001]            # build the image, run it, 23 checks
test/test-compose.sh [--port-base 58100]       # three nodes, tokens, network, teardown
test/test-deb.sh --deb FILE --work WORK        # install/upgrade/remove/purge in systemd
```

All three build from a clean snapshot of `HEAD` plus the working-tree
`kingres/packaging` (`--context DIR` overrides). `test-deb.sh` starts a clean
`ubuntu:24.04` with systemd as PID 1 (`--privileged`, host cgroup namespace;
`policy-rc.d` removed), installs the package with `apt`, and checks: dependency
resolution; users, owners and modes; the service enabled, active and running as
`kingres`; the API (`uninitialized`, token 0600, panel, favicon); the
`kingres-*` commands; a throw-away server with both extensions; the sandbox
(no capabilities, `NoNewPrivs`, no write outside the data directory);
restart after `kill -9`; token kept across restarts; an upgrade to revision 2
(data, token and local configuration kept); removal (data kept, files gone);
reinstall (same token); purge (everything gone).

## Assumptions about `kingresd` (to be confirmed by the control-plane engineer)

1. Settings come from `-config` JSON, `KINGRES_*` variables and flags, as the
   control-plane README says. The shipped `kingresd.json` uses only keys that
   exist today (`listen`, `data-dir`, `log-format`, `log-level`); the file is
   rejected if it has an unknown key, so nothing else is written there.
2. The data directory is `/var/lib/kingres` (package) or the `KINGRES_DATA_DIR`
   volume (image); instances will live in `<data-dir>/instances/<node id>/`
   (decision D3), the setup token in `<data-dir>/setup-token`.
3. **`KINGRES_PG_BIN_DIR=/opt/kingres/bin`** is set by the unit and the image;
   `kingresd` does not read it yet (D3 names a `pg-bin-dir` setting but the key
   is not implemented). `PATH` also starts with `/opt/kingres/bin`. When the real
   setting exists, change the unit, the image and `kingresd.json`.
4. Instances should put their Unix sockets in `/run/kingres` (the unit has
   `PrivateTmp=yes`, so `/tmp` sockets are invisible to other processes) and
   listen on TCP for clients.
5. The unit has no `ExecReload` and no `Type=notify`: `kingresd` does not call
   `sd_notify`. `kingresd` handles `SIGTERM` (graceful stop, exit code 0, tested
   in the image).
6. Instances started by `kingresd` stay in the unit's cgroup, so they stop with the
   service (`TimeoutStopSec=120`); if `kingresd` should leave instances running
   across a restart, the unit needs `KillMode=process` or a separate slice.
7. In the container `kingresd` must listen on `0.0.0.0`; there it is protected by
   the setup token (the localhost-only default of the architecture document cannot
   be kept behind a published port).
8. The unit name is `kingres.service` (as requested); the architecture document says
   `kingresdb.service`.

## What is not included yet

- **No Patroni, no etcd, no metrics store** (decided: they arrive with the
  high-availability phase; `kingresd` exposes Prometheus-format metrics itself).
- `kingresd` does not manage PostgreSQL instances yet, so a fresh package or image
  has the wizard and the API but no database server running; the server programs
  are installed and tested (`test/pg-smoke.sh`).
- Joining nodes (compose only prepares the topology); `kingres-cli`.
- RPM, arm64, Windows; package signing and an apt repository; image signing and
  publishing; SBOM; debug symbols package (`-dbgsym`); a `kingres-dev` package;
  man pages beyond one-page stubs; the licences of every compiled-in Go and npm
  dependency as full texts (only the dependency lists are shipped); NLS, GSSAPI,
  LDAP, PAM, JIT, PL languages.
- Package upgrades are tested for a revision bump only (no database migration,
  no major-version path, no rollback).

## What was run

Timings and sizes are those of one run on a 24-core WSL2 machine;
they vary with caches and network.

Built from commit `30875ac0b08d5a2a2291eb67fc111c410daba9d8` (the last commit at
the time; the snapshot plus the not yet committed `kingres/packaging`).

| Measurement | Value |
|---|---|
| Release recipe, warm Go/npm caches, 24 cores | 23-25 s (server compile included) |
| Staged tree `/opt/kingres` | 41 MB (installed size of the package 41.9 MB) |
| `kingres_0.1.0-1_amd64.deb` | 10,426,468 bytes (9.9 MiB), xz |
| Image `kingres:test`, first build with cold Docker cache | 264 s (apt installs dominate) |
| Image size | 264 MB on disk (Ubuntu 24.04 base 119 MB), 70.0 MB compressed content |
| Image start to healthy | a few seconds; `docker stop` 1 s, exit code 0 |
| Package test (install, upgrade, remove, purge) | 59-62 s |
| Compose test (build, three nodes up, teardown) | 83 s |

| Check | Result |
|---|---|
| `build/build.sh` from a snapshot of `30875ac0b0` | ran, OK; a second build in the same work directory gave a bit-identical `.deb` |
| Build from `852be15fbc` (no `kingres/verifier`) | ran: the hook skips `kingres-verify`, package builds |
| `deb/build-deb.sh --lint` (lintian 2.117) | 0 errors, 0 warnings; 139 information tags (listed above) |
| `test/test-deb.sh` | ran in an `ubuntu:24.04` container with systemd as PID 1: 61 checks passed |
| `test/test-image.sh` | ran: 23 checks passed |
| `test/test-compose.sh` | ran: 16 checks passed |
| `shellcheck -x -P SCRIPTDIR` on every script | clean |
| `actionlint` 1.7.12 (with shellcheck) on `ci.yml` | clean; **the workflow was not executed on GitHub** |
| Redocly 2.78.0 lint and the rule checker on `openapi.yaml` | passed locally (the same commands as the CI job) |

---

Copyright (c) 2026, RooyeKhat Media Co. Released under the PostgreSQL License.
