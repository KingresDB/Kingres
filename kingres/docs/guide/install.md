<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Installing KingresDB

KingresDB is released for two PostgreSQL majors at the same time. Choose one; a cluster runs one major, and the package manager refuses to mix them.

| | PostgreSQL 18 | PostgreSQL 19 |
|---|---|---|
| Product branch | `kingres-18` | `kingres-19` |
| Git tag of a release | `kingres-<version>-pg18` | `kingres-<version>-pg19` |
| Debian package | `kingres_<version>+pg18-<revision>_amd64.deb` | `kingres_<version>+pg19-<revision>_amd64.deb` |
| Container image | `kingres:<version>-pg18` | `kingres:<version>-pg19` |
| What the server reports | `18.x (KingresDB <version>)` | `19.x (KingresDB <version>)` |

The bases are PostgreSQL 18.7 (`kingres-18`, upstream tag `REL_18_7`) and PostgreSQL 19.0 (`kingres-19`, upstream tag `REL_19_0`, the branch moved from `REL_19_RC2` on 2026-11-22; [../dev/releases.md](../dev/releases.md)).

## Download a release

KingresDB 0.1.0 was released on 2026-11-28. Each major has its own release page on GitHub:

| | Release page | Tag |
|---|---|---|
| PostgreSQL 18 | [kingres-0.1.0-pg18](https://github.com/KingresDB/Kingres/releases/tag/kingres-0.1.0-pg18) | `kingres-0.1.0-pg18` on `kingres-18` |
| PostgreSQL 19 | [kingres-0.1.0-pg19](https://github.com/KingresDB/Kingres/releases/tag/kingres-0.1.0-pg19) | `kingres-0.1.0-pg19` on `kingres-19` |

Each page carries the Debian package (`kingres_0.1.0+pg<N>-1_amd64.deb`), the image archive (`kingres-0.1.0-pg<N>-image.tar.gz`, for `docker load`) and `SHA256SUMS`. The page of PostgreSQL 18 also carries the files of the standalone verifier: the binaries for linux-amd64, linux-arm64, darwin-arm64 and windows-amd64, `RELEASE`, and the signed sums of the verifier under the names `kingres-verify-SHA256SUMS` and `kingres-verify-SHA256SUMS.note`.

Check the files before you install anything. The lines of `SHA256SUMS` name the files with the folder of their major (`pg18/...` or `pg19/...`), so put the downloaded files into that folder next to `SHA256SUMS`, and let `sha256sum` skip the files of the other major:

```
mkdir -p kingres-0.1.0/pg18 && cd kingres-0.1.0
# download SHA256SUMS here, and the .deb and the image archive into pg18/
sha256sum -c --ignore-missing SHA256SUMS      # every line that is checked must read OK
sudo apt install ./pg18/kingres_0.1.0+pg18-1_amd64.deb
# or: gunzip -c pg18/kingres-0.1.0-pg18-image.tar.gz | docker load
```

`SHA256SUMS` protects against a damaged download. It is not signed in 0.1.0 (later releases sign it), and there is no apt repository, so it does not protect against a release page that someone else controls.

From the release after 0.1.0, the sums of the packages and images are named `kingres-SHA256SUMS`, list the files without a folder, and come with `kingres-SHA256SUMS.note`, a note signed with the same release key as the verifier's files; check them with `kingres-verify release verify` as below ([../../packaging/README.md](../../packaging/README.md#checking-the-files-of-a-release)).

The verifier files are signed. Check them with a verifier you already trust (a build of your own, or one from an earlier release) and with the release key printed in [../../verifier/README.md](../../verifier/README.md) and on the wiki page of the verifier, not with a key from the place you downloaded the files:

```
kingres-verify release verify --sums kingres-verify-SHA256SUMS --note kingres-verify-SHA256SUMS.note \
    --key <release key> --binary kingres-verify-linux-amd64
```

The line you want is `VERIFIED`. The `kingres-verify` that the package installs is the same program; the verifier README describes the command and its exit statuses.

## From the Debian package

Supported: Ubuntu 24.04 on amd64 (the package is built there; other distributions are not tested). Install it with apt so that the run-time libraries come along:

```
sudo apt install ./kingres_<version>+pg18-<revision>_amd64.deb
```

What it does:

- Puts the product under `/opt/kingres` and links the commands into `/usr/bin`: `kingresd`, `kingres-verify` and the clients with a `kingres-` prefix (`kingres-psql`, `kingres-pg_dump`, `kingres-pg_dumpall`, `kingres-pg_restore`, `kingres-pg_basebackup`, `kingres-pg_isready`, `kingres-pg_config`). The prefix keeps them apart from the `psql` of the distribution. The server programs (`postgres`, `initdb`, `pg_ctl`) are not on the `PATH`; `kingresd` runs them from `/opt/kingres/bin`.
- Installs the service `kingres.service` (runs as the user `kingres`, never root) and starts it. The node is then uninitialized and its console listens on `127.0.0.1:8091` only.
- Keeps settings in `/etc/kingres/kingresd.json` and secrets in `/etc/kingres/kingresd.env` (mode 0600). Both are conffiles: your changes survive an upgrade.
- Keeps all state in `/var/lib/kingres` (the database in `instances/1/pgdata`, the certificates in `pki/`, the audit log). The server log is in `instances/1/pgdata/log/`, and the log of `kingresd` is in the journal (`journalctl -u kingres`).

Then create or join a cluster: [quickstart.md](quickstart.md).

**Only one major per machine.** The package `kingres` has the same name for both majors, and `+pg19` sorts after `+pg18`, so apt would offer one as an upgrade of the other. The package's `preinst` script refuses it before anything is unpacked, when a package or a data directory of the other major is on the machine. There is no procedure yet to move a cluster between majors ([upgrade.md](upgrade.md)).

**Settings you may need to change before the first run:**

| Need | Setting |
|---|---|
| The console reachable from other machines | `KINGRES_LISTEN=0.0.0.0:8091` in `kingresd.env`, and TLS with `KINGRES_TLS_CERT_FILE` and `KINGRES_TLS_KEY_FILE` (readable by the `kingres` user) |
| Setup from another machine while the node is uninitialized | `KINGRES_ALLOW_REMOTE_SETUP=true` (the setup token is still required) |
| Another database port (the distribution's PostgreSQL uses 5432) | `pg-port` in `kingresd.json` |
| Several servers | `server-host`, `internal-listen` (the console port plus one), `pg-listen`: see [quickstart.md](quickstart.md) |
| Prometheus scraping | `KINGRES_METRICS_TOKEN` in `kingresd.env`; metrics are at `/metrics` |

All settings of `kingresd` are listed in [../../control-plane/README.md](../../control-plane/README.md), "Configuration".

**Service control.**

| What | Command |
|---|---|
| Restart everything, database included | `sudo systemctl restart kingres` |
| Restart only the manager; the database keeps running and connections stay open | `sudo systemctl kill --kill-whom=main --signal=SIGTERM kingres` |
| Stop everything | `sudo systemctl stop kingres` |

**Removing.** `apt remove kingres` stops the node and removes the programs; data, settings and the user stay, and installing again brings the same cluster back. `apt purge kingres` also deletes `/var/lib/kingres` (every database of the node, the certificates, the audit log). Take a backup first.

## From a container image

The image is `kingres:<version>-pg18` or `kingres:<version>-pg19`. There is no tag without the major. Load the image archive of a release ([above](#download-a-release)), or build it from a product branch (`kingres/packaging/docker/build-image.sh --rev kingres-18`; needs Docker):

```
gunzip -c kingres-<version>-pg18-image.tar.gz | docker load
docker run -d --name kingres --stop-timeout 120 \
  -p 127.0.0.1:8091:8091 -p 127.0.0.1:5432:5432 \
  -v kingres-data:/var/lib/kingres kingres:<version>-pg18
docker exec kingres cat /var/lib/kingres/setup-token
```

**Publish the console port and the database port on 127.0.0.1 only, as above.** The image listens on `0.0.0.0:8091` over plain HTTP and accepts setup from other machines (`KINGRES_LISTEN`, `KINGRES_ALLOW_REMOTE_SETUP=true`, `KINGRES_PG_LISTEN=0.0.0.0`); only the setup token guards it. `docker run -p 8091:8091` on a network that is not trusted sends the setup token and the first administrator password in clear text. Bind to `127.0.0.1` as the compose file does, or put TLS in front (`KINGRES_TLS_CERT_FILE`, `KINGRES_TLS_KEY_FILE`). The PostgreSQL port 5432 also listens on `0.0.0.0` inside the container (`KINGRES_PG_LISTEN`): publish it on `127.0.0.1` or on a private network only, and add rules on the network access page before other machines connect.

Open `http://127.0.0.1:8091/`, enter the token and create the cluster. Give `docker stop` time to shut the database down (`--stop-timeout 120`); use a volume with one major only. The image runs with a read-only root file system and no capabilities. By default the database accepts no network login: add rules on the console's network access page before other containers connect. The full list of settings, the volume layout and the three-node compose cluster are in [../../packaging/README.md](../../packaging/README.md).

## From source

Two ways: build exactly what a release contains, or build a development tree.

### The release build

On Ubuntu 24.04, as an unprivileged user, with the build packages of `kingres/dev/setup-ubuntu.sh` plus `libsystemd-dev libxml2-dev liburing-dev libnuma-dev uuid-dev dpkg-dev file`, and the pinned Go and Node.js (`kingres/packaging/build/toolchain.sh` installs them and checks their checksums):

```
git clone <repository> && cd <repository>
kingres/packaging/build/toolchain.sh ~/toolchain
export PATH=~/toolchain/go/bin:~/toolchain/node/bin:$PATH

# PostgreSQL 18 (use --rev kingres-19 for PostgreSQL 19; the recipe is the same)
kingres/packaging/build/build.sh --rev kingres-18 --work ~/kingres-build18
kingres/packaging/deb/build-deb.sh --work ~/kingres-build18       # the .deb is in ~/kingres-build18/deb
sudo apt install ~/kingres-build18/deb/kingres_*+pg18-1_amd64.deb
```

`build.sh` exports a clean snapshot of the revision (uncommitted work is never built), builds the server, the clients and every `contrib/kingres_*` extension with meson (optimized, assertions off), builds the console and `kingresd` with the console embedded, builds `kingres-verify`, and stages the tree under `/opt/kingres`. The tree it was given decides the major. Always build from a branch or a commit.

To stay at the staged tree without a package, copy `<work>/stage/opt/kingres` to `/opt/kingres` on a machine that has the run-time libraries and run `kingresd` yourself (`kingresd -data-dir /var/lib/kingres -pg-bin-dir /opt/kingres/bin`). The package does more for you (user, unit, directories), so prefer it.

### A development tree

For hacking on KingresDB, not for running a cluster: [../dev/getting-started.md](../dev/getting-started.md). It builds with assertions, installs into a private prefix and runs the test suites. The extensions need `shared_preload_libraries = 'kingres_shard'` and `max_prepared_transactions` above zero on every node when you run them by hand; `kingresd` sets both on the instances it manages.

## After installing

- Check the version: `kingresd -version`, and `SELECT version();` in `kingres-psql` (the answer ends with `(KingresDB <version>)`).
- The extensions of a database: `\dx` in `kingres-psql`. The five extensions are `kingres_shard`, `kingres_ledger`, `kingres_columnar`, `kingres_sketch` and `kingres_timepart`. When a database is created, `kingresd` creates `kingres_shard` or `kingres_ledger` in it when sharding or the ledger is on, and also `kingres_columnar` and `kingres_timepart` (and the same on a server that is added later). `kingres_sketch` is installed by the task `database.approximateAggregates` ([operator-guide.md](operator-guide.md), section 11) or with `CREATE EXTENSION`. A database made by an earlier version gets `kingres_columnar` and `kingres_timepart` at the first storage or policy operation.
- `kingresd` writes `shared_preload_libraries = 'kingres_shard,kingres_ledger,kingres_columnar,pg_stat_statements'`; a library that the installation has no file for is left out (`kingres_columnar` and `pg_stat_statements` are the optional ones; an instance that has had `kingres_columnar` in its list keeps it, and does not start until the package that has the file is installed). The list takes effect at a restart of the instance; the console's restart of pending instances orders standbys before primaries ([../../packaging/README.md](../../packaging/README.md), "The preload of `kingres_columnar`"). A conversion to columnar storage is refused until every instance of every server has loaded the library.
